// Package backlog is each user's list of ideas for developing the assistant later: something it
// should do differently or learn to do. Unlike memory, the backlog never reaches the prompt and
// changes nothing the assistant does; the model reads it only through backlog_list, and the user
// sees it with /backlog and exports it to paste into a development session.
package backlog

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
)

const (
	StatusOpen = "open"
	StatusDone = "done"

	MaximumTitleRunes   = 120
	MaximumDetailsRunes = 2000
	MaximumOpenItems    = 200
)

type Item struct {
	ID          string
	PrincipalID string
	Title       string
	Details     string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Repository interface {
	// ListBacklog returns the principal's items with the status, oldest first.
	ListBacklog(ctx context.Context, principalID, status string) ([]Item, error)
	LoadBacklogItem(ctx context.Context, principalID, id string) (Item, bool, error)
	AddBacklogItem(context.Context, Item) error
	// UpdateBacklogItem saves the item's title, details, status, and update time.
	UpdateBacklogItem(context.Context, Item) (bool, error)
	RemoveBacklogItem(ctx context.Context, principalID, id string) (bool, error)
}

type Config struct {
	// Now is the clock for item timestamps; nil means time.Now.
	Now func() time.Time
}

type Service struct {
	repository Repository
	proposer   Proposer
	exposure   Exposure
	logger     *slog.Logger
	now        func() time.Time

	// mu serialises writes so the cap on open items holds when a turn's tool calls run in
	// parallel. It is never held while proposing, because an approval executes under its own locks
	// and takes mu.
	mu sync.Mutex
}

func NewService(cfg Config, repository Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("backlog repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("backlog logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, proposer: proposer, logger: logger, now: now}, nil
}

// SetExposure sets who answers whether a conversation has read outside content, at wiring time.
func (s *Service) SetExposure(exposure Exposure) { s.exposure = exposure }

func (s *Service) Now() time.Time { return s.now() }

// Items returns the principal's open items, oldest first.
func (s *Service) Items(ctx context.Context, principalID string) ([]Item, error) {
	return s.repository.ListBacklog(ctx, principalID, StatusOpen)
}

// MarkDone closes one of the principal's open items; false means it is not open any more.
func (s *Service) MarkDone(ctx context.Context, principalID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found, err := s.repository.LoadBacklogItem(ctx, principalID, id)
	if err != nil || !found || item.Status != StatusOpen {
		return false, err
	}
	item.Status, item.UpdatedAt = StatusDone, s.now()
	return s.repository.UpdateBacklogItem(ctx, item)
}

func (s *Service) Remove(ctx context.Context, principalID, id string) (bool, error) {
	return s.repository.RemoveBacklogItem(ctx, principalID, id)
}

// rejection is why a write cannot be made, in words for the model; it is empty when it can.
type rejection string

// add stores a new open item unless the principal's open items are at the cap.
func (s *Service) add(ctx context.Context, principalID, title, details string) (Item, rejection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	open, err := s.repository.ListBacklog(ctx, principalID, StatusOpen)
	if err != nil {
		return Item{}, "", err
	}
	if len(open) >= MaximumOpenItems {
		return Item{}, fullText, nil
	}
	id, err := newID()
	if err != nil {
		return Item{}, "", err
	}
	now := s.now()
	item := Item{ID: id, PrincipalID: principalID, Title: title, Details: details, Status: StatusOpen, CreatedAt: now, UpdatedAt: now}
	if err := s.repository.AddBacklogItem(ctx, item); err != nil {
		return Item{}, "", err
	}
	return item, "", nil
}

// change is a complete replacement of an item's editable fields.
type change struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Details string `json:"details"`
	Status  string `json:"status"`
}

// update saves the change, unless the item is gone or reopening it would pass the cap.
func (s *Service) update(ctx context.Context, principalID string, next change) (rejection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found, err := s.repository.LoadBacklogItem(ctx, principalID, next.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return unknownItemText, nil
	}
	if item.Status != StatusOpen && next.Status == StatusOpen {
		open, err := s.repository.ListBacklog(ctx, principalID, StatusOpen)
		if err != nil {
			return "", err
		}
		if len(open) >= MaximumOpenItems {
			return fullText, nil
		}
	}
	item.Title, item.Details, item.Status, item.UpdatedAt = next.Title, next.Details, next.Status, s.now()
	if _, err := s.repository.UpdateBacklogItem(ctx, item); err != nil {
		return "", err
	}
	return "", nil
}

var (
	fullText        rejection = rejection(fmt.Sprintf("The backlog already holds %d open items. Ask the user to mark some done or delete them in /backlog first.", MaximumOpenItems))
	unknownItemText rejection = "No backlog item has that id. Use backlog_list to find it."
)

func newID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate backlog identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// normalizeTitle makes a title a single line.
func normalizeTitle(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// normalizeDetails keeps the details' line breaks, which carry lists and steps, but not runs of
// blank lines or trailing spaces.
func normalizeDetails(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" && (len(kept) == 0 || kept[len(kept)-1] == "") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func validTitle(title string) string {
	switch {
	case title == "":
		return "title must not be empty."
	case !utf8.ValidString(title):
		return "title must be valid UTF-8."
	case utf8.RuneCountInString(title) > MaximumTitleRunes:
		return fmt.Sprintf("title is %d characters; it holds at most %d. Shorten it and keep the rest in details.", utf8.RuneCountInString(title), MaximumTitleRunes)
	}
	return ""
}

func validDetails(details string) string {
	switch {
	case !utf8.ValidString(details):
		return "details must be valid UTF-8."
	case utf8.RuneCountInString(details) > MaximumDetailsRunes:
		return fmt.Sprintf("details are %d characters; they hold at most %d. Shorten them.", utf8.RuneCountInString(details), MaximumDetailsRunes)
	}
	return ""
}
