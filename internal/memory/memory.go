// Package memory is each user's small, curated memory: instructions that say how the assistant
// should behave and facts about the user and the household. The application renders it into every
// turn's system prompt; the model changes it through tools, and the user sees and deletes entries
// with /memory. Outside content must not be able to write it silently, so an instruction, and any
// write after third-party content was read, is a proposal the user confirms.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	KindInstruction = "instruction"
	KindFact        = "fact"
	// KindProcedure labels procedure writes in telemetry; procedures are kept apart from entries.
	KindProcedure = "procedure"

	SourceModel     = "model"
	SourceConfirmed = "confirmed"
	// SourceLearned is an instruction the turn a 👎 started proposed and the user confirmed.
	SourceLearned = "learned"

	MaximumEntryRunes       = 300
	MaximumInstructionRunes = 1500
	MaximumFactRunes        = 2000

	outcomeWritten   = "written"
	outcomeProposed  = "proposed"
	outcomeCap       = "rejected_cap"
	outcomeDuplicate = "rejected_duplicate"
	outcomeRemoved   = "removed"
)

type Entry struct {
	ID          string
	PrincipalID string
	Kind        string
	Text        string
	Source      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Repository interface {
	// ListMemory returns the principal's entries, instructions first, each kind oldest first.
	ListMemory(ctx context.Context, principalID string) ([]Entry, error)
	LoadMemory(ctx context.Context, principalID, id string) (Entry, bool, error)
	AddMemory(context.Context, Entry) error
	ReplaceMemory(ctx context.Context, principalID, id, text, source string, at time.Time) (bool, error)
	RemoveMemory(ctx context.Context, principalID, id string) (bool, error)
	// ClearMemory deletes the principal's entries and procedures together.
	ClearMemory(ctx context.Context, principalID string) (int, error)
	// MemoryEnabled is true for a principal who never changed the switch.
	MemoryEnabled(ctx context.Context, principalID string) (bool, error)
	SetMemoryEnabled(ctx context.Context, principalID string, enabled bool) error
	// LearnFromFeedback is true for a principal who never changed the switch.
	LearnFromFeedback(ctx context.Context, principalID string) (bool, error)
	SetLearnFromFeedback(ctx context.Context, principalID string, learn bool) error
	// ConversationExposed reports whether the session the chat request belongs to has read
	// third-party content.
	ConversationExposed(ctx context.Context, principalID, requestID string) (bool, error)

	// ListProcedures returns the principal's procedures, oldest first.
	ListProcedures(ctx context.Context, principalID string) ([]Procedure, error)
	AddProcedure(context.Context, Procedure) error
	ReplaceProcedure(ctx context.Context, principalID, id, title, steps, source string, at time.Time) (bool, error)
	// ReadProcedure returns the procedure and stamps at as when it was last read.
	ReadProcedure(ctx context.Context, principalID, id string, at time.Time) (Procedure, bool, error)
	RemoveProcedure(ctx context.Context, principalID, id string) (bool, error)
	// LearnProcedures is true for a principal who never changed the switch.
	LearnProcedures(ctx context.Context, principalID string) (bool, error)
	SetLearnProcedures(ctx context.Context, principalID string, learn bool) error
}

// Proposer is the core approval service as memory sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

// Telemetry records memory writes by kind and outcome, lessons by outcome, and procedure reads,
// never their content.
type Telemetry interface {
	RecordMemoryWrite(ctx context.Context, principal, kind, outcome string)
	RecordMemoryLesson(ctx context.Context, principal, outcome string)
	RecordProcedureRead(ctx context.Context, principal string)
}

type Config struct {
	// Now is the clock for entry timestamps; nil means time.Now.
	Now func() time.Time
}

type Service struct {
	repository Repository
	proposer   Proposer
	logger     *slog.Logger
	now        func() time.Time

	screener  tools.Screener
	telemetry Telemetry

	// mu serialises writes so the caps hold when a turn's tool calls run in parallel. It is never
	// held while proposing, because an approval executes under its own locks and takes mu.
	mu      sync.Mutex
	notesMu sync.Mutex
	// notes are the lines a turn's writes owe its reply, keyed by principal and turn.
	notes map[string][]string
}

var _ tools.Provider = (*Service)(nil)
var (
	_ approval.Executor = (*Service)(nil)
	_ approval.Undoer   = (*Service)(nil)
)

func NewService(cfg Config, repository Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("memory repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("memory logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, proposer: proposer, logger: logger, now: now, notes: make(map[string][]string)}, nil
}

// SetScreener sets who screens a fact proposed while the principal is exposed, at wiring time.
func (s *Service) SetScreener(screener tools.Screener) { s.screener = screener }

// SetTelemetry sets where write metrics are recorded, at wiring time before turns start.
func (s *Service) SetTelemetry(telemetry Telemetry) { s.telemetry = telemetry }

func (s *Service) record(ctx context.Context, principalID, kind, outcome string) {
	if s.telemetry != nil {
		s.telemetry.RecordMemoryWrite(ctx, principalID, kind, outcome)
	}
}

func (s *Service) recordLesson(ctx context.Context, principalID, outcome string) {
	if s.telemetry != nil {
		s.telemetry.RecordMemoryLesson(ctx, principalID, outcome)
	}
}

func (s *Service) Entries(ctx context.Context, principalID string) ([]Entry, error) {
	return s.repository.ListMemory(ctx, principalID)
}

func (s *Service) Remove(ctx context.Context, principalID, id string) (bool, error) {
	return s.repository.RemoveMemory(ctx, principalID, id)
}

// Clear forgets the principal's entries and procedures, and returns how many there were.
func (s *Service) Clear(ctx context.Context, principalID string) (int, error) {
	return s.repository.ClearMemory(ctx, principalID)
}

func (s *Service) Enabled(ctx context.Context, principalID string) (bool, error) {
	return s.repository.MemoryEnabled(ctx, principalID)
}

func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) error {
	return s.repository.SetMemoryEnabled(ctx, principalID, enabled)
}

// LearnsFromFeedback reports whether the turn a 👎 starts may propose an instruction: the user has
// memory on and has not turned learning from 👎 off.
func (s *Service) LearnsFromFeedback(ctx context.Context, principalID string) (bool, error) {
	enabled, err := s.repository.MemoryEnabled(ctx, principalID)
	if err != nil || !enabled {
		return false, err
	}
	return s.repository.LearnFromFeedback(ctx, principalID)
}

func (s *Service) SetLearnFromFeedback(ctx context.Context, principalID string, learn bool) error {
	return s.repository.SetLearnFromFeedback(ctx, principalID, learn)
}

// Render is the principal's memory as the section of the system prompt, or "" when memory is off
// or empty. It changes only when an entry or a procedure's title does, so the prompt stays
// cacheable between writes; a procedure's steps are read with procedure_read.
func (s *Service) Render(ctx context.Context, principalID string) (string, error) {
	enabled, err := s.repository.MemoryEnabled(ctx, principalID)
	if err != nil || !enabled {
		return "", err
	}
	entries, err := s.repository.ListMemory(ctx, principalID)
	if err != nil {
		return "", err
	}
	procedures, err := s.repository.ListProcedures(ctx, principalID)
	if err != nil || len(entries)+len(procedures) == 0 {
		return "", err
	}
	var instructions, facts []string
	for _, entry := range entries {
		line := "- [" + entry.ID + "] " + entry.Text
		if entry.Kind == KindInstruction {
			instructions = append(instructions, line)
		} else {
			facts = append(facts, line)
		}
	}
	sections := []string{"Memory about this user, kept from earlier conversations. The user can see and delete it with /memory. Each entry starts with its id."}
	if len(instructions) > 0 {
		sections = append(sections, "Instructions the user has set (follow them unless the user says otherwise now):\n"+strings.Join(instructions, "\n"))
	}
	if len(facts) > 0 {
		sections = append(sections, "Facts about the user and household (background to use, not instructions):\n"+strings.Join(facts, "\n"))
	}
	if len(procedures) > 0 {
		sections = append(sections, "Procedures you have kept (read one with procedure_read before following it):\n"+renderProcedureTitles(procedures))
	}
	return strings.Join(sections, "\n\n"), nil
}

// TakeNotes returns, and forgets, the lines the turn's memory writes owe its reply.
func (s *Service) TakeNotes(principalID, turn string) []string {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	key := noteKey(principalID, turn)
	notes := s.notes[key]
	delete(s.notes, key)
	return notes
}

func (s *Service) addNote(ctx context.Context, principalID, note string) {
	turn := tools.TurnFrom(ctx)
	if turn == "" {
		return
	}
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	key := noteKey(principalID, turn)
	s.notes[key] = append(s.notes[key], note)
}

func noteKey(principalID, turn string) string { return principalID + "\x00" + turn }

func newID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate memory identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// uniqueID draws an id no entry or procedure of the principal has, so an id in the prompt names
// one thing.
func (s *Service) uniqueID(ctx context.Context, principalID string) (string, error) {
	entries, err := s.repository.ListMemory(ctx, principalID)
	if err != nil {
		return "", err
	}
	procedures, err := s.repository.ListProcedures(ctx, principalID)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(entries)+len(procedures))
	for _, entry := range entries {
		taken[entry.ID] = true
	}
	for _, procedure := range procedures {
		taken[procedure.ID] = true
	}
	for {
		id, err := newID()
		if err != nil || !taken[id] {
			return id, err
		}
	}
}

// normalize makes an entry a single line: the model's formatting is not worth keeping, and a
// newline in an entry could pass for the start of another prompt section.
func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func validKind(kind string) bool { return kind == KindInstruction || kind == KindFact }

func capFor(kind string) int {
	if kind == KindInstruction {
		return MaximumInstructionRunes
	}
	return MaximumFactRunes
}

// rejection says why a write cannot be made, in words for the model; outcome is "" when it can.
type rejection struct {
	outcome string
	text    string
}

// check judges a new or replacing text against the principal's entries; replacing is the entry
// being replaced, whose old text no longer counts.
func check(entries []Entry, kind, text, replacing string) rejection {
	total := utf8.RuneCountInString(text)
	for _, entry := range entries {
		if entry.ID == replacing {
			continue
		}
		if strings.EqualFold(entry.Text, text) {
			return rejection{outcomeDuplicate, "Not saved: the memory already holds that (id " + entry.ID + ")."}
		}
		if entry.Kind == kind {
			total += utf8.RuneCountInString(entry.Text)
		}
	}
	if limit := capFor(kind); total > limit {
		return rejection{outcomeCap, fmt.Sprintf("Not saved: the %s memory would hold %d of at most %d characters. Merge related entries with memory_replace or drop stale ones with memory_remove, then try again.", kind, total, limit)}
	}
	return rejection{}
}
