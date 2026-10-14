// Package attachment keeps the photos and files a principal sends in a conversation. Bytes live
// on disk under the principal's own directory and never enter the database, which holds only the
// metadata a turn needs. An image is passed to the model on the turn that carried it; any other
// file is read through a tool, so a request that only files a document never pays for its content.
package attachment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// Limits are fixed rather than configured: they exist to bound one household chat, and a value
// large enough to matter would be refused by the model or by Telegram first.
const (
	MaximumImageBytes   = 5 << 20
	MaximumFileBytes    = 20 << 20
	MaximumSessionFiles = 10
	MaximumSessionBytes = 50 << 20

	maximumReadBytes = 12 << 10
	maximumNameRunes = 200
	maximumMIMERunes = 100
)

type Kind string

const (
	KindImage Kind = "image"
	KindFile  Kind = "file"
)

// Incoming is one downloaded file on its way into a conversation.
type Incoming struct {
	Kind     Kind
	FileName string
	MimeType string
	Data     []byte
}

// Record is the stored metadata of one attachment; the bytes stay on disk.
type Record struct {
	ID          string
	PrincipalID string
	SessionID   string
	RequestID   string
	Position    int
	Kind        Kind
	FileName    string
	MimeType    string
	SizeBytes   int64
}

// SessionRef names one principal's conversation session.
type SessionRef struct {
	PrincipalID string
	SessionID   string
}

// Room is what a principal's active session still has space for.
type Room struct {
	Files int
	Bytes int64
}

// Turn is what one request's attachments contribute to a turn: the images the model looks at and
// the text describing every file it may read.
type Turn struct {
	Images  []agentruntime.Image
	Summary string
}

type Repository interface {
	ListRequestAttachments(context.Context, string, string) ([]Record, error)
	ListSessionAttachments(context.Context, string, string) ([]Record, error)
	LoadAttachment(context.Context, string, string) (Record, bool, error)
	ActiveAttachmentSession(context.Context, string) (string, bool, error)
	DeleteSessionAttachments(context.Context, string, string) error
	PurgeClosedAttachments(context.Context) ([]SessionRef, error)
}

type Config struct {
	Directory string
}

type Service struct {
	root       string
	repository Repository
	logger     *slog.Logger
}

func NewService(cfg Config, repository Repository, logger *slog.Logger) (*Service, error) {
	if strings.TrimSpace(cfg.Directory) == "" {
		return nil, fmt.Errorf("attachment directory is required")
	}
	if repository == nil {
		return nil, fmt.Errorf("attachment repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("attachment logger is required")
	}
	root, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment directory: %w", err)
	}
	if err := preparePrivateDirectory(root); err != nil {
		return nil, err
	}
	return &Service{root: root, repository: repository, logger: logger}, nil
}

// Room reports how much the principal's active session may still accept, so an oversized or
// overfull request is refused before anything is downloaded.
func (s *Service) Room(ctx context.Context, principalID string) (Room, error) {
	sessionID, found, err := s.repository.ActiveAttachmentSession(ctx, principalID)
	if err != nil {
		return Room{}, err
	}
	if !found {
		return Room{Files: MaximumSessionFiles, Bytes: MaximumSessionBytes}, nil
	}
	records, err := s.repository.ListSessionAttachments(ctx, principalID, sessionID)
	if err != nil {
		return Room{}, err
	}
	room := Room{Files: MaximumSessionFiles - len(records), Bytes: MaximumSessionBytes}
	for _, record := range records {
		room.Bytes -= record.SizeBytes
	}
	if room.Files < 0 {
		room.Files = 0
	}
	if room.Bytes < 0 {
		room.Bytes = 0
	}
	return room, nil
}

// Stage writes one request's bytes to disk and returns the rows that describe them. The rows are
// recorded by the transaction that enqueues the request, so a turn never starts between the
// request becoming visible and its files being stored.
func (s *Service) Stage(principalID, requestID string, files []Incoming) ([]Record, error) {
	if len(files) == 0 {
		return nil, nil
	}
	directory, err := s.principalDirectory(principalID)
	if err != nil {
		return nil, err
	}
	if err := preparePrivateDirectory(directory); err != nil {
		return nil, err
	}

	records := make([]Record, 0, len(files))
	for position, file := range files {
		if len(file.Data) == 0 {
			return nil, fmt.Errorf("attachment %q is empty", loggableName(file.FileName))
		}
		id, err := newID()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(directory, id), file.Data, 0o600); err != nil {
			return nil, fmt.Errorf("write attachment: %w", err)
		}
		records = append(records, Record{
			ID:          id,
			PrincipalID: principalID,
			RequestID:   requestID,
			Position:    position,
			Kind:        file.Kind,
			FileName:    boundedName(file.FileName),
			MimeType:    boundedMIME(file.MimeType),
			SizeBytes:   int64(len(file.Data)),
		})
	}
	return records, nil
}

// Discard removes staged bytes the enqueue did not record, such as a redelivered update or one
// refused as busy. What it cannot remove is left for Purge.
func (s *Service) Discard(records []Record) {
	for _, record := range records {
		if path, err := s.path(record); err == nil {
			_ = os.Remove(path)
			_ = os.Remove(path + textCacheSuffix)
		}
	}
}

// Turn loads what one request's attachments contribute to its turn.
func (s *Service) Turn(ctx context.Context, principalID, requestID string) (Turn, error) {
	records, err := s.repository.ListRequestAttachments(ctx, principalID, requestID)
	if err != nil {
		return Turn{}, err
	}
	if len(records) == 0 {
		return Turn{}, nil
	}

	var turn Turn
	var lines []string
	for _, record := range records {
		if record.Kind == KindImage {
			data, err := s.read(record)
			if err != nil {
				lines = append(lines, fmt.Sprintf("- %s (image) is no longer available", record.FileName))
				continue
			}
			turn.Images = append(turn.Images, agentruntime.Image{
				Name:     record.FileName,
				MimeType: record.MimeType,
				Data:     data,
			})
			lines = append(lines, fmt.Sprintf("- %s (image, id %s, attached to this message)", record.FileName, record.ID))
			continue
		}
		lines = append(lines, "- "+describe(record))
	}
	turn.Summary = "[Attached by the user]\n" + strings.Join(lines, "\n")
	return turn, nil
}

// CloseSession removes everything a closed session held.
func (s *Service) CloseSession(ctx context.Context, principalID, sessionID string) error {
	records, err := s.repository.ListSessionAttachments(ctx, principalID, sessionID)
	if err != nil {
		return err
	}
	if err := s.repository.DeleteSessionAttachments(ctx, principalID, sessionID); err != nil {
		return err
	}
	s.Discard(records)
	return nil
}

// Purge reconciles disk with the database at start: the rows of closed sessions are deleted, and
// every file that no longer belongs to a stored attachment is removed, including the cached text of
// one whose bytes are gone. Bytes staged by a request that was never enqueued are collected here.
func (s *Service) Purge(ctx context.Context) error {
	active, err := s.repository.PurgeClosedAttachments(ctx)
	if err != nil {
		return err
	}
	keep := make(map[string]map[string]struct{}, len(active))
	for _, ref := range active {
		directory, err := s.principalDirectory(ref.PrincipalID)
		if err != nil {
			continue
		}
		records, err := s.repository.ListSessionAttachments(ctx, ref.PrincipalID, ref.SessionID)
		if err != nil {
			return err
		}
		names, seen := keep[directory]
		if !seen {
			names = make(map[string]struct{}, len(records))
			keep[directory] = names
		}
		for _, record := range records {
			names[record.ID] = struct{}{}
			names[record.ID+textCacheSuffix] = struct{}{}
		}
	}

	principals, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read attachment directory: %w", err)
	}
	for _, principal := range principals {
		if !principal.IsDir() {
			continue
		}
		directory := filepath.Join(s.root, principal.Name())
		files, err := os.ReadDir(directory)
		if err != nil {
			return fmt.Errorf("read attachment directory: %w", err)
		}
		for _, file := range files {
			if _, kept := keep[directory][file.Name()]; kept {
				continue
			}
			if err := os.RemoveAll(filepath.Join(directory, file.Name())); err != nil {
				return fmt.Errorf("remove attachment file: %w", err)
			}
		}
	}
	return nil
}

// Describe returns the record of one of the principal's attachments without reading its bytes,
// so a connector can name a file in a proposal without paying for its content.
func (s *Service) Describe(ctx context.Context, principalID, attachmentID string) (Record, bool, error) {
	if !idPattern.MatchString(attachmentID) {
		return Record{}, false, nil
	}
	record, found, err := s.repository.LoadAttachment(ctx, principalID, attachmentID)
	if err != nil || !found {
		return Record{}, false, err
	}
	return record, true, nil
}

// Content returns the stored bytes of one of the principal's attachments with its record, so a
// connector can file a file the user sent without reaching into this package's directory layout.
// A missing attachment, or one belonging to another principal, reports false rather than an error.
func (s *Service) Content(ctx context.Context, principalID, attachmentID string) (Record, []byte, bool, error) {
	if !idPattern.MatchString(attachmentID) {
		return Record{}, nil, false, nil
	}
	record, found, err := s.repository.LoadAttachment(ctx, principalID, attachmentID)
	if err != nil || !found {
		return Record{}, nil, false, err
	}
	data, err := s.read(record)
	if err != nil {
		return Record{}, nil, false, nil
	}
	return record, data, true, nil
}

func (s *Service) read(record Record) ([]byte, error) {
	path, err := s.path(record)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	return data, nil
}

func (s *Service) path(record Record) (string, error) {
	directory, err := s.principalDirectory(record.PrincipalID)
	if err != nil {
		return "", err
	}
	if !idPattern.MatchString(record.ID) {
		return "", fmt.Errorf("attachment identifier is invalid")
	}
	return filepath.Join(directory, record.ID), nil
}

func (s *Service) principalDirectory(principalID string) (string, error) {
	name, err := agentruntime.ProfileDirectoryName(principalID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, name), nil
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// describe is the one line the model reads about a file it has not opened.
func describe(record Record) string {
	line := fmt.Sprintf("%s (%s, %s, id %s)", record.FileName, record.MimeType, humanSize(record.SizeBytes), record.ID)
	if readableAsText(record.MimeType, record.FileName) || isPDF(record.MimeType, record.FileName) {
		return line + " — read it with read_attachment only if the request needs its content"
	}
	return line + " — its content cannot be read as text"
}

func humanSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%d KB", size/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// readableAsText reports whether a file is worth offering as text. The MIME type Telegram
// reports is the sender's claim, so the extension is a fallback rather than a contradiction.
func readableAsText(mimeType, fileName string) bool {
	base := baseMIME(mimeType)
	if strings.HasPrefix(base, "text/") {
		return true
	}
	switch base {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/toml", "application/csv", "application/javascript", "application/x-sh",
		"application/sql", "application/x-ndjson":
		return true
	}
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".jsonl", ".yaml", ".yml", ".xml",
		".log", ".ini", ".toml", ".conf", ".sh", ".sql", ".go", ".py", ".js", ".ts", ".html", ".css":
		return true
	}
	return false
}

// isPDF applies the same trust as readableAsText: the sender's claim first, the extension second.
func isPDF(mimeType, fileName string) bool {
	return baseMIME(mimeType) == "application/pdf" || strings.EqualFold(filepath.Ext(fileName), ".pdf")
}

func baseMIME(mimeType string) string {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mimeType)), ";")
	return strings.TrimSpace(base)
}

func boundedName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || !utf8.ValidString(cleaned) {
		return "file"
	}
	return truncateRunes(cleaned, maximumNameRunes)
}

func boundedMIME(mimeType string) string {
	cleaned := strings.TrimSpace(mimeType)
	if cleaned == "" || !utf8.ValidString(cleaned) || strings.IndexFunc(cleaned, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return "application/octet-stream"
	}
	return truncateRunes(cleaned, maximumMIMERunes)
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

// loggableName keeps a sender-chosen filename out of an error that may be logged.
func loggableName(name string) string {
	if utf8.RuneCountInString(name) > 40 || strings.IndexFunc(name, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return "file"
	}
	return name
}

func newID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate attachment identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func preparePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("attachment directory %q must be a real directory", filepath.Base(path))
		}
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("attachment directory %q permissions are %04o, want 0700", filepath.Base(path), info.Mode().Perm())
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create attachment directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure attachment directory: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("inspect attachment directory: %w", err)
	}
}
