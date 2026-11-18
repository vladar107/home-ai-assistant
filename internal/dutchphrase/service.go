package dutchphrase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

const (
	recentPhrases     = 100
	maximumCandidates = 5
	maximumReplyBytes = 16 << 10
)

var heading = locale.T("🇳🇱 **Dutch phrase of the day**", "🇳🇱 **Фраза дня на нидерландском**")

// Checker runs the phrase's tool-less turn on the principal's own subscription; the application
// adapts the turn engine to it.
type Checker interface {
	RunPhrase(ctx context.Context, principalID, message string) (text, errorKind string, err error)
}

// Config is what the service needs. Preferences give the user's interface language, which the
// translations and the block's labels are in.
type Config struct {
	Repository  Repository
	Checker     Checker
	Preferences locale.Preferences
	Logger      *slog.Logger
	Now         func() time.Time
}

// Service produces a user's Dutch phrase of the day.
type Service struct {
	repository  Repository
	checker     Checker
	preferences locale.Preferences
	logger      *slog.Logger
	now         func() time.Time
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Repository == nil || cfg.Checker == nil || cfg.Preferences == nil {
		return nil, fmt.Errorf("the Dutch phrase needs a repository, a checker, and language preferences")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: cfg.Repository, checker: cfg.Checker, preferences: cfg.Preferences, logger: logger, now: now}, nil
}

// Phrase returns the block that ends the principal's Heads-up scheduled for scheduledFor, whose
// local date in location the phrase belongs to, or "" when there is nothing to add. Only the run
// that stores the day's phrase adds it; a manual run shows the day's phrase again. A failed or
// unreadable turn, or one whose candidates were all sent before, gives "" and is logged; an error
// is a store failure.
func (s *Service) Phrase(ctx context.Context, principalID string, scheduledFor time.Time, location *time.Location, notes string, manual bool) (string, error) {
	if location == nil {
		location = time.UTC
	}
	date := scheduledFor.In(location).Format(DateLayout)
	userCtx, err := locale.ForPrincipal(ctx, s.preferences, principalID)
	if err != nil {
		s.logger.WarnContext(ctx, "dutchphrase.language_unread", "principal_id", principalID, "error", err)
	}
	existing, found, err := s.repository.LoadDutchPhrase(ctx, principalID, date)
	if err != nil {
		return "", err
	}
	if found {
		return s.again(userCtx, existing, manual), nil
	}
	recent, err := s.repository.RecentDutchPhrases(ctx, principalID, recentPhrases)
	if err != nil {
		return "", err
	}
	message := prompt.DutchPhrase(scheduledFor, location, locale.From(userCtx), recent, notes)
	text, errorKind, err := s.checker.RunPhrase(userCtx, principalID, message)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.logger.WarnContext(ctx, "dutchphrase.failed", "principal_id", principalID, "error", err)
		return "", nil
	}
	if errorKind != "" {
		s.logger.WarnContext(ctx, "dutchphrase.failed", "principal_id", principalID, "error_kind", errorKind)
		return "", nil
	}
	candidates, ok := decode(text)
	if !ok {
		s.logger.WarnContext(ctx, "dutchphrase.unreadable", "principal_id", principalID)
		return "", nil
	}
	valid := make([]Phrase, 0, len(candidates))
	normalized := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		phrase := candidate.phrase(principalID, date)
		if !phrase.Valid() {
			continue
		}
		valid = append(valid, phrase)
		normalized = append(normalized, phrase.Normalized)
	}
	sent, err := s.repository.SentDutchPhrases(ctx, principalID, normalized)
	if err != nil {
		return "", err
	}
	rank := -1
	for index, phrase := range valid {
		if !sent[phrase.Normalized] {
			rank = index
			break
		}
	}
	if rank < 0 {
		s.logger.WarnContext(ctx, "dutchphrase.none_usable", "principal_id", principalID, "candidates", len(candidates), "valid", len(valid))
		return "", nil
	}
	chosen := valid[rank]
	chosen.CreatedAt = s.now().UTC()
	inserted, err := s.repository.InsertDutchPhrase(ctx, chosen)
	if err != nil {
		return "", err
	}
	if !inserted {
		// Another run stored a phrase for the day meanwhile, or the same phrase for another day.
		existing, found, err := s.repository.LoadDutchPhrase(ctx, principalID, date)
		if err != nil {
			return "", err
		}
		if !found {
			s.logger.WarnContext(ctx, "dutchphrase.conflict", "principal_id", principalID)
			return "", nil
		}
		return s.again(userCtx, existing, manual), nil
	}
	s.logger.InfoContext(ctx, "dutchphrase.chosen", "principal_id", principalID, "candidates", len(candidates), "valid", len(valid), "rank", rank)
	return render(userCtx, chosen), nil
}

// again is the block of a day that already has its phrase, which only a manual run shows.
func (s *Service) again(ctx context.Context, phrase Phrase, manual bool) string {
	if !manual {
		return ""
	}
	return render(ctx, phrase)
}

// render writes the block from a stored phrase. The fields were checked to hold no markup, so the
// phrase and the translations can stand inside bold and spoilers.
func render(ctx context.Context, phrase Phrase) string {
	lines := []string{
		heading.Format(ctx),
		"**" + phrase.Phrase + "**",
		phrase.Usage,
		"||" + phrase.Translation + " — " + phrase.UsageTranslation + "||",
	}
	if phrase.Note != "" {
		lines = append(lines, "||"+phrase.Note+"||")
	}
	return strings.Join(lines, "\n")
}

type candidate struct {
	Phrase           string `json:"phrase"`
	Usage            string `json:"usage"`
	Translation      string `json:"translation"`
	UsageTranslation string `json:"usage_translation"`
	Note             string `json:"note"`
}

func (c candidate) phrase(principalID, date string) Phrase {
	text := strings.TrimSpace(c.Phrase)
	return Phrase{
		PrincipalID:      principalID,
		LocalDate:        date,
		Phrase:           text,
		Normalized:       Normalize(text),
		Usage:            strings.TrimSpace(c.Usage),
		Translation:      strings.TrimSpace(c.Translation),
		UsageTranslation: strings.TrimSpace(c.UsageTranslation),
		Note:             strings.TrimSpace(c.Note),
	}
}

// decode reads the turn's reply, tolerating a code fence around it, and returns at most
// maximumCandidates candidates in the model's order.
func decode(text string) ([]candidate, bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimPrefix(text, "json")
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	if text == "" || len(text) > maximumReplyBytes {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	var reply struct {
		Candidates []candidate `json:"candidates"`
	}
	if err := decoder.Decode(&reply); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	if len(reply.Candidates) > maximumCandidates {
		reply.Candidates = reply.Candidates[:maximumCandidates]
	}
	return reply.Candidates, true
}
