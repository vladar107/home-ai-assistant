// Package homeassistant is the Home Assistant connector: for a principal whose home switch is on,
// their turns reach the devices the household exposed to Assist through Home Assistant's own MCP
// tools, read energy statistics, and propose confirmed service calls for devices labelled as
// needing confirmation, all with one household token.
package homeassistant

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration    = "homeassistant"
	DefaultBaseURL = "https://ha.home"
)

type Config struct {
	// Token is the household's long-lived access token, created by a non-admin Home Assistant user.
	Token string
	// BaseURL is Home Assistant's address; empty means DefaultBaseURL.
	BaseURL string
	// Now is the clock for the snapshot's age; nil means time.Now.
	Now func() time.Time
}

// Proposer is the core approval service as the connector sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// ExposureChecker reports whether the calling conversation or run has read content written outside
// the household.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

type Service struct {
	client   *client
	switches integration.SwitchReader
	proposer Proposer
	exposure ExposureChecker
	logger   *slog.Logger
	now      func() time.Time

	snapshotMu sync.Mutex
	snapshot   *snapshot

	running atomic.Bool
}

var (
	_ module.Module        = (*Service)(nil)
	_ integration.Switched = (*Service)(nil)
)

func NewService(cfg Config, switches integration.SwitchReader, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if switches == nil {
		return nil, fmt.Errorf("home assistant switches are required")
	}
	if logger == nil {
		return nil, fmt.Errorf("home assistant logger is required")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, fmt.Errorf("home assistant token is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" {
		return nil, fmt.Errorf("home assistant URL must be an http or https origin without credentials, path, or query")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		client:   newClient(parsed, token),
		switches: switches,
		proposer: proposer,
		logger:   logger,
		now:      now,
	}, nil
}

// SetExposure makes immediate home actions need confirmation once the checker reports outside
// content; nil, the default, never gates them. Call it before the service handles turns.
func (s *Service) SetExposure(checker ExposureChecker) {
	s.exposure = checker
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: "0.1.0", Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("home assistant connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.client.http.CloseIdleConnections()
	return nil
}

// Switch is each principal's home switch. The home is on by default wherever the operator
// configured a token, since the home is shared.
var Switch = integration.Switch{Integration: Integration, Name: "The home", DefaultOn: true}

func (s *Service) Switch() integration.Switch { return Switch }

func (s *Service) enabled(ctx context.Context, principalID string) (bool, error) {
	return s.switches.Enabled(ctx, principalID, Switch)
}

// Exposure is what the exposure check found: whether it could run, and the exposed devices that
// need confirmation, which withhold every immediate home action while they stay exposed.
type Exposure struct {
	Checked   bool
	Conflicts []string
}

// Exposure reports the check for the settings page; an unreachable Home Assistant is reported as
// unchecked rather than as an error.
func (s *Service) Exposure(ctx context.Context) Exposure {
	snap, err := s.current(ctx)
	if err != nil || !snap.guarded {
		return Exposure{}
	}
	return Exposure{Checked: true, Conflicts: snap.conflicts}
}
