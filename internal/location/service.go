package location

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/module"
)

const moduleVersion = "0.1.0"

// Describer names the place at a point, such as "Stationsplein 1, Leiden"; an empty name means
// none is known.
type Describer interface {
	Describe(ctx context.Context, latitude, longitude float64) (string, error)
}

// Policy is how long a shared position stays usable.
type Policy struct {
	// PinTTL is how long a pin or venue lasts after it was shared.
	PinTTL time.Duration
	// LiveSilence ends a live share shared until the user stops it once no update came for this long.
	LiveSilence time.Duration
	// LiveGrace keeps the last position of a live share this long after it ended.
	LiveGrace time.Duration
}

// ExpiresAt is when a fix stops being usable. It is computed from the policy at read time, so a
// changed setting applies to positions already stored.
func (p Policy) ExpiresAt(fix Fix) time.Time {
	if fix.Source != SourceLive {
		return fix.UpdatedAt.Add(p.PinTTL)
	}
	end := fix.EndedAt
	if !fix.LiveUntil.IsZero() && (end.IsZero() || fix.LiveUntil.Before(end)) {
		end = fix.LiveUntil
	}
	if end.IsZero() {
		return fix.UpdatedAt.Add(p.LiveSilence)
	}
	return end.Add(p.LiveGrace)
}

type Config struct {
	Policy        Policy
	PruneInterval time.Duration
	// Describer names places; nil leaves them unnamed.
	Describer Describer
	// Zone is where times are shown, the household's time zone.
	Zone *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type Service struct {
	repository    Repository
	describer     Describer
	zone          *time.Location
	policy        Policy
	pruneInterval time.Duration
	now           func() time.Time
	logger        *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, repository Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("location repository is required")
	}
	if cfg.Policy.PinTTL <= 0 || cfg.Policy.LiveSilence <= 0 || cfg.Policy.LiveGrace < 0 || cfg.PruneInterval <= 0 {
		return nil, fmt.Errorf("location durations must be positive")
	}
	if cfg.Zone == nil {
		return nil, fmt.Errorf("location time zone is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository:    repository,
		describer:     cfg.Describer,
		zone:          cfg.Zone,
		policy:        cfg.Policy,
		pruneInterval: cfg.PruneInterval,
		now:           now,
		logger:        logger,
	}, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "location", Version: moduleVersion, Kind: module.KindSystem}
}

func (s *Service) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.stopped = make(chan struct{})
	go s.pruneLoop(ctx, s.stopped)
	return nil
}

func (s *Service) Ready(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return fmt.Errorf("location service is not running")
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, stopped := s.cancel, s.stopped
	s.cancel, s.stopped = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) pruneLoop(ctx context.Context, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(s.pruneInterval)
	defer ticker.Stop()
	for {
		if err := s.Prune(ctx); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "location.prune.failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Prune deletes every position that is no longer usable.
func (s *Service) Prune(ctx context.Context) error {
	fixes, err := s.repository.ListLocations(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, fix := range fixes {
		if now.Before(s.policy.ExpiresAt(fix)) {
			continue
		}
		if err := s.repository.DeleteLocation(ctx, fix.PrincipalID, fix.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

// Share stores a newly shared position and returns the acknowledgement for the user.
func (s *Service) Share(ctx context.Context, fix Fix) (string, error) {
	if err := fix.Validate(); err != nil {
		return "", err
	}
	if err := s.repository.PutLocation(ctx, fix); err != nil {
		return "", err
	}
	near := s.describe(ctx, fix)
	now := s.now()
	switch {
	case fix.Source == SourceLive && fix.LiveUntil.IsZero():
		return "📍 Following your live location until you stop sharing it" + nearSuffix(near) + ".", nil
	case fix.Source == SourceLive:
		return "📍 Following your live location until " + s.clock(fix.LiveUntil, now) + nearSuffix(near) + ".", nil
	case near != "":
		return "📍 Got it, near " + near + ". Ask me anything about it.", nil
	default:
		return "📍 Got it. Ask me anything about it.", nil
	}
}

// Move applies an edit of a live location message and reports whether it was stored.
func (s *Service) Move(ctx context.Context, fix Fix) (bool, error) {
	if err := fix.Validate(); err != nil {
		return false, err
	}
	if fix.Source != SourceLive {
		return false, fmt.Errorf("only a live location moves")
	}
	return s.repository.MoveLiveLocation(ctx, fix)
}

// Current is the principal's usable position, if any.
func (s *Service) Current(ctx context.Context, principalID string) (Fix, bool, error) {
	fix, found, err := s.repository.LoadLocation(ctx, principalID)
	if err != nil || !found {
		return Fix{}, false, err
	}
	if !s.now().Before(s.policy.ExpiresAt(fix)) {
		return Fix{}, false, nil
	}
	return fix, true, nil
}

// Forget removes the principal's position and reports whether one was stored.
func (s *Service) Forget(ctx context.Context, principalID string) (bool, error) {
	return s.repository.ForgetLocation(ctx, principalID, s.now())
}

// TurnLine tells a turn where its user is; empty when no usable position is stored.
func (s *Service) TurnLine(ctx context.Context, principalID string) (string, error) {
	fix, found, err := s.Current(ctx, principalID)
	if err != nil || !found {
		return "", err
	}
	now := s.now()
	var builder strings.Builder
	builder.WriteString("User's location: ")
	builder.WriteString(Coordinates(fix.Latitude, fix.Longitude))
	var details []string
	if fix.Accuracy > 0 {
		details = append(details, "±"+strconv.Itoa(int(math.Round(fix.Accuracy)))+" m")
	}
	switch {
	case fix.Live(now) && fix.LiveUntil.IsZero():
		details = append(details, "live, updated "+ago(now.Sub(fix.UpdatedAt)))
	case fix.Live(now):
		details = append(details, "live until "+s.clock(fix.LiveUntil, now)+", updated "+ago(now.Sub(fix.UpdatedAt)))
	case fix.Source == SourceLive:
		details = append(details, "live share ended, last updated "+ago(now.Sub(fix.UpdatedAt)))
	default:
		details = append(details, "shared "+ago(now.Sub(fix.UpdatedAt)))
	}
	builder.WriteString(" (" + strings.Join(details, "; ") + ")")
	if fix.VenueTitle != "" {
		builder.WriteString(", at " + oneLine(fix.VenueTitle))
	}
	if near := s.describe(ctx, fix); near != "" {
		builder.WriteString(", near " + near)
	}
	builder.WriteString(".")
	return builder.String(), nil
}

// Summary is the settings line for the principal's position.
func (s *Service) Summary(ctx context.Context, principalID string) (string, bool, error) {
	fix, found, err := s.Current(ctx, principalID)
	if err != nil || !found {
		return "Not shared", false, err
	}
	now := s.now()
	var text string
	switch {
	case fix.Live(now) && fix.LiveUntil.IsZero():
		text = "Shared: live until you stop it"
	case fix.Live(now):
		text = "Shared: live until " + s.clock(fix.LiveUntil, now)
	default:
		text = "Shared " + ago(now.Sub(fix.UpdatedAt))
	}
	return text + nearSuffix(s.describe(ctx, fix)), true, nil
}

// describe names a fix's place: the venue's own address, else the describer's answer. A failed
// lookup leaves it unnamed rather than failing the turn.
func (s *Service) describe(ctx context.Context, fix Fix) string {
	if fix.VenueAddress != "" {
		return oneLine(fix.VenueAddress)
	}
	if s.describer == nil {
		return ""
	}
	name, err := s.describer.Describe(ctx, fix.Latitude, fix.Longitude)
	if err != nil {
		s.logger.WarnContext(ctx, "location.describe.failed", "principal_id", fix.PrincipalID, "error", err)
		return ""
	}
	return oneLine(name)
}

// Coordinates formats a point as the "lat, lon" the tools accept.
func Coordinates(latitude, longitude float64) string {
	return strconv.FormatFloat(latitude, 'f', 5, 64) + ", " + strconv.FormatFloat(longitude, 'f', 5, 64)
}

func nearSuffix(near string) string {
	if near == "" {
		return ""
	}
	return ", near " + near
}

func (s *Service) clock(at, now time.Time) string {
	at, now = at.In(s.zone), now.In(s.zone)
	if at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return at.Format("15:04")
	}
	return at.Format("Mon 15:04")
}

func ago(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return strconv.Itoa(max(0, int(elapsed/time.Second))) + " s ago"
	case elapsed < time.Hour:
		return strconv.Itoa(int(elapsed/time.Minute)) + " min ago"
	default:
		return strconv.FormatFloat(elapsed.Hours(), 'f', 1, 64) + " h ago"
	}
}

func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return text
}
