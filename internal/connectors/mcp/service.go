package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	// integrationPrefix names a catalog server's integration: mcp-<server>.
	integrationPrefix = "mcp-"

	defaultIntakeLifetime = 10 * time.Minute
	// callTimeout bounds one tool call and each HTTP exchange of a session.
	callTimeout = 30 * time.Second
	// verifyTimeout bounds checking a key with initialize and tools/list.
	verifyTimeout = 15 * time.Second
	// closeTimeout bounds ending a replaced session, which only the server benefits from.
	closeTimeout = 2 * time.Second

	minimumKeyLength = 16
	maximumKeyLength = 512
	maximumTitleLen  = 200
	// consumedPerPrincipal bounds the messages remembered per principal after an intake consumed
	// them, so a replayed one is consumed again.
	consumedPerPrincipal = 8
)

var (
	ErrUnknownServer = errors.New("the MCP server is not in the catalog")
	ErrNotAllowed    = errors.New("the MCP server is not available to this principal")
	ErrNoKeyNeeded   = errors.New("the MCP server takes no key")
	ErrInvalidKey    = errors.New("the message is not a key for the MCP server")
	ErrKeyRefused    = errors.New("the MCP server refused the key")
	// ErrKeyUnverified is a key that could not be checked because the server failed otherwise.
	ErrKeyUnverified = errors.New("the MCP server could not verify the key")
	// ErrSwitchUnchanged is a key that was verified and stored while the server could not be
	// turned on; the status returned with it shows the switch as it is.
	ErrSwitchUnchanged = errors.New("the key is stored but the MCP server could not be turned on")

	// errRetired refuses a session for a key read before it was removed or replaced, or after
	// shutdown.
	errRetired = errors.New("the MCP session was retired")
)

// SwitchStore reads and changes principals' integration switches; *integration.Switches is one.
type SwitchStore interface {
	integration.SwitchReader
	SetEnabled(ctx context.Context, principalID string, integration integration.Switch, enabled bool) error
}

type Config struct {
	// Servers is the operator catalog, in the order its tools and rows are offered.
	Servers     []config.MCPServer
	Credentials *integration.Credentials
	Repository  integration.Repository
	Switches    SwitchStore
	Logger      *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// RefreshTimeout bounds refreshing one tool list; zero means three seconds.
	RefreshTimeout time.Duration
	// IntakeLifetime bounds how long a key intake waits for the key; zero means ten minutes.
	IntakeLifetime time.Duration
}

// Service is the MCP connector: one tool provider per catalog server, the per-principal sessions
// and cached tool lists behind them, and the key lifecycle the settings section drives.
type Service struct {
	servers        []config.MCPServer
	providers      []*provider
	byName         map[string]*provider
	credentials    *integration.Credentials
	repository     integration.Repository
	switches       SwitchStore
	logger         *slog.Logger
	now            func() time.Time
	refreshTimeout time.Duration
	intakeLifetime time.Duration
	running        atomic.Bool

	mu sync.Mutex
	// idle is signalled when the last background task ends.
	idle      *sync.Cond
	tasks     int
	lifecycle context.Context
	stop      context.CancelFunc
	// epoch and generations retire sessions: a session is stored only under the generation read
	// before its key was, and forget and Shutdown move them on.
	epoch       uint64
	generations map[sessionKey]uint64
	clients     map[sessionKey]*openClient
	// retiring are replaced sessions still finishing their calls before they are closed.
	retiring   map[sessionKey][]*openClient
	lists      map[sessionKey]*listEntry
	problems   map[sessionKey]bool
	unreadable map[sessionKey]bool
	intakes    map[string]pendingIntake
	// consumed are the messages an intake consumed, per principal, by a keyed fingerprint that a
	// random per-process secret makes useless outside this process.
	consumed      map[string][]consumedMessage
	consumeSecret []byte
}

type consumedMessage struct {
	mark   string
	at     time.Time
	result IntakeResult
}

// sessionKey names one principal's use of one server; a list of a server without keys is shared
// and keyed with an empty principal.
type sessionKey struct {
	server    string
	principal string
}

type generation struct {
	epoch uint64
	count uint64
}

// openClient is a stored session. users counts the calls using it; it is closed only after they
// end, so no request carries its key once retire returns.
type openClient struct {
	client      *Client
	fingerprint string
	users       sync.WaitGroup
	retired     sync.Once
}

// retire waits for the session's calls and ends it; a second caller waits for the first.
func (c *openClient) retire() {
	c.retired.Do(func() {
		c.users.Wait()
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		c.client.Close(closeCtx)
	})
}

type pendingIntake struct {
	server  string
	expires time.Time
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Credentials == nil:
		return nil, fmt.Errorf("MCP credentials are required")
	case cfg.Repository == nil:
		return nil, fmt.Errorf("MCP repository is required")
	case cfg.Switches == nil:
		return nil, fmt.Errorf("MCP switches are required")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("MCP logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	refresh := cfg.RefreshTimeout
	if refresh <= 0 {
		refresh = defaultRefreshTimeout
	}
	lifetime := cfg.IntakeLifetime
	if lifetime <= 0 {
		lifetime = defaultIntakeLifetime
	}
	s := &Service{
		servers:        append([]config.MCPServer(nil), cfg.Servers...),
		byName:         make(map[string]*provider, len(cfg.Servers)),
		credentials:    cfg.Credentials,
		repository:     cfg.Repository,
		switches:       cfg.Switches,
		logger:         cfg.Logger,
		now:            now,
		refreshTimeout: refresh,
		intakeLifetime: lifetime,
		generations:    make(map[sessionKey]uint64),
		clients:        make(map[sessionKey]*openClient),
		retiring:       make(map[sessionKey][]*openClient),
		lists:          make(map[sessionKey]*listEntry),
		problems:       make(map[sessionKey]bool),
		unreadable:     make(map[sessionKey]bool),
		intakes:        make(map[string]pendingIntake),
		consumed:       make(map[string][]consumedMessage),
		consumeSecret:  make([]byte, 32),
	}
	if _, err := rand.Read(s.consumeSecret); err != nil {
		return nil, fmt.Errorf("MCP intake secret: %w", err)
	}
	s.idle = sync.NewCond(&s.mu)
	for _, server := range s.servers {
		if _, duplicate := s.byName[server.Name]; duplicate {
			return nil, fmt.Errorf("MCP server %q appears twice", server.Name)
		}
		p := newProvider(s, server)
		if !integration.ValidName(p.sw.Integration) {
			return nil, fmt.Errorf("MCP server %q cannot name an integration", server.Name)
		}
		s.providers = append(s.providers, p)
		s.byName[server.Name] = p
	}
	return s, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "mcp", Version: "0.1.0", Kind: module.KindConnector}
}

// Start lets sessions open and warms the lists of the servers without keys in the background.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.lifecycle, s.stop = context.WithCancel(context.WithoutCancel(ctx))
	s.running.Store(true)
	s.mu.Unlock()
	for _, server := range s.servers {
		if server.Auth != config.MCPAuthKey {
			s.warm(server)
		}
	}
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("MCP connector is not running")
	}
	return nil
}

// Shutdown cancels the background refreshes, waits for them and for the calls in flight, and ends
// every session as best effort, within ctx. No session opens afterwards.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.running.Store(false)
	s.epoch++
	stop := s.stop
	var open []*openClient
	for _, current := range s.clients {
		open = append(open, current)
	}
	for _, replaced := range s.retiring {
		open = append(open, replaced...)
	}
	s.clients = make(map[sessionKey]*openClient)
	s.lists = make(map[sessionKey]*listEntry)
	s.intakes = make(map[string]pendingIntake)
	s.consumed = make(map[string][]consumedMessage)
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, current := range open {
			wg.Go(current.retire)
		}
		wg.Wait()
		s.waitIdle()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// track runs task in the background under the service's lifecycle, unless the service is not
// running.
func (s *Service) track(task func(lifecycle context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running.Load() {
		return false
	}
	s.goLocked(func() { task(s.lifecycle) })
	return true
}

// goLocked runs task in the background as a tracked task; the caller holds s.mu.
func (s *Service) goLocked(task func()) {
	s.tasks++
	go func() {
		defer func() {
			s.mu.Lock()
			s.tasks--
			if s.tasks == 0 {
				s.idle.Broadcast()
			}
			s.mu.Unlock()
		}()
		task()
	}()
}

func (s *Service) waitIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.tasks > 0 {
		s.idle.Wait()
	}
}

// Providers are the tool providers, one per catalog server in catalog order; each is
// integration.Switched.
func (s *Service) Providers() []tools.Provider {
	providers := make([]tools.Provider, 0, len(s.providers))
	for _, p := range s.providers {
		providers = append(providers, p)
	}
	return providers
}

// Switches are the servers' integration switches in catalog order.
func (s *Service) Switches() []integration.Switch {
	switches := make([]integration.Switch, 0, len(s.providers))
	for _, p := range s.providers {
		switches = append(switches, p.sw)
	}
	return switches
}

// IntegrationName is the integration a catalog server's switch, key file, and connection record
// are named by.
func IntegrationName(server string) string {
	return integrationPrefix + server
}

// credential is what a principal sends to a server: nothing for a server without keys.
type credential struct {
	value       string
	fingerprint string
}

// KeyState is where a principal's key for a keyed server stands.
type KeyState string

const (
	// KeyNotNeeded is a server that takes no key.
	KeyNotNeeded KeyState = "not_needed"
	KeyMissing   KeyState = "missing"
	KeyConnected KeyState = "connected"
	// KeyStale is a key recorded for another origin than the catalog's: it is never sent.
	KeyStale KeyState = "stale"
	// KeyRefused is a connected key the server refused since it was last accepted.
	KeyRefused KeyState = "refused"
)

// credentialFor returns what the principal sends to the server and the state of their key. A key
// is usable only while its file and its connection record both exist and the record's origin is
// the catalog's.
func (s *Service) credentialFor(ctx context.Context, principalID string, server config.MCPServer) (credential, KeyState, error) {
	if server.Auth != config.MCPAuthKey {
		return credential{}, KeyNotNeeded, nil
	}
	name := IntegrationName(server.Name)
	key, found, err := s.credentials.Read(principalID, name)
	if err != nil {
		if s.firstUnreadable(sessionKey{server.Name, principalID}) {
			s.logger.WarnContext(ctx, "mcp.credentials.unreadable", "principal_id", principalID, "server", server.Name)
		}
		return credential{}, KeyMissing, nil
	}
	record, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, name)
	if err != nil {
		return credential{}, KeyMissing, err
	}
	switch {
	case !found || !bound:
		return credential{}, KeyMissing, nil
	case record.WorkspaceID != server.Origin():
		return credential{}, KeyStale, nil
	}
	state := KeyConnected
	if s.hasProblem(sessionKey{server.Name, principalID}) {
		state = KeyRefused
	}
	return credential{value: key, fingerprint: fingerprint(key)}, state, nil
}

func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Service) firstUnreadable(key sessionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unreadable[key] {
		return false
	}
	s.unreadable[key] = true
	return true
}

// generation is read before a key, and a session is stored only while it is unchanged.
func (s *Service) generation(key sessionKey) generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return generation{epoch: s.epoch, count: s.generations[key]}
}

// clientFor returns the session for the key and a release the caller runs when its call ends. It
// refuses with errRetired when the service is not running or the generation moved on, so a key
// read before it was removed cannot open a session. A session for another key is replaced and
// retired in the background once its calls end.
func (s *Service) clientFor(key sessionKey, server config.MCPServer, cred credential, gen generation) (*Client, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running.Load() || gen != (generation{epoch: s.epoch, count: s.generations[key]}) {
		return nil, nil, errRetired
	}
	current := s.clients[key]
	if current != nil && current.fingerprint == cred.fingerprint {
		current.users.Add(1)
		return current.client, current.users.Done, nil
	}
	client, err := newClient(server, cred.value)
	if err != nil {
		return nil, nil, err
	}
	fresh := &openClient{client: client, fingerprint: cred.fingerprint}
	fresh.users.Add(1)
	s.clients[key] = fresh
	if current != nil {
		s.retiring[key] = append(s.retiring[key], current)
		s.goLocked(func() {
			current.retire()
			s.mu.Lock()
			defer s.mu.Unlock()
			s.retiring[key] = slices.DeleteFunc(s.retiring[key], func(c *openClient) bool { return c == current })
			if len(s.retiring[key]) == 0 {
				delete(s.retiring, key)
			}
		})
	}
	return client, fresh.users.Done, nil
}

func newClient(server config.MCPServer, key string) (*Client, error) {
	options := Options{URL: server.URL, Timeout: callTimeout}
	if server.Auth == config.MCPAuthKey {
		options.HeaderName = server.KeyHeader
		options.HeaderValue = server.KeyPrefix + key
	}
	return New(options)
}

// forget retires the principal's session and drops their cached list, problem, and warnings for
// the server. It returns once every call on the session, and on any session it replaced, has
// ended and the session is closed, so no request carries the old key afterwards.
func (s *Service) forget(principalID string, server config.MCPServer) {
	key := sessionKey{server.Name, principalID}
	s.mu.Lock()
	s.generations[key]++
	sessions := slices.Clone(s.retiring[key])
	if current := s.clients[key]; current != nil {
		sessions = append(sessions, current)
	}
	delete(s.clients, key)
	delete(s.lists, key)
	delete(s.problems, key)
	delete(s.unreadable, key)
	s.mu.Unlock()
	for _, session := range sessions {
		session.retire()
	}
}

func (s *Service) markProblem(ctx context.Context, principalID string, server config.MCPServer) {
	if server.Auth != config.MCPAuthKey {
		return
	}
	s.mu.Lock()
	key := sessionKey{server.Name, principalID}
	known := s.problems[key]
	s.problems[key] = true
	s.mu.Unlock()
	if !known {
		s.logger.WarnContext(ctx, "mcp.key.refused", "principal_id", principalID, "server", server.Name)
	}
}

func (s *Service) clearProblem(principalID string, server config.MCPServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.problems, sessionKey{server.Name, principalID})
}

func (s *Service) hasProblem(key sessionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.problems[key]
}

// State is a server's row as the settings section shows it.
type State string

const (
	StateOff            State = "off"
	StateOn             State = "on"
	StateNeedsKey       State = "needs_key"
	StateNeedsReconnect State = "needs_reconnect"
	StateProblem        State = "problem"
)

// ServerStatus is the non-secret view of one server for one principal.
type ServerStatus struct {
	Name        string
	Title       string
	Description string
	// Keyed reports that the server takes each user's own key.
	Keyed   bool
	Enabled bool
	Key     KeyState
	State   State
}

// Servers lists the servers the principal may use, in catalog order.
func (s *Service) Servers(ctx context.Context, principalID string) ([]ServerStatus, error) {
	var statuses []ServerStatus
	for _, p := range s.providers {
		if !p.server.Allows(principalID) {
			continue
		}
		status, err := s.status(ctx, principalID, p)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Status reports one server for the principal.
func (s *Service) Status(ctx context.Context, principalID, server string) (ServerStatus, error) {
	p, err := s.allowed(principalID, server)
	if err != nil {
		return ServerStatus{}, err
	}
	return s.status(ctx, principalID, p)
}

func (s *Service) status(ctx context.Context, principalID string, p *provider) (ServerStatus, error) {
	enabled, err := s.switches.Enabled(ctx, principalID, p.sw)
	if err != nil {
		return ServerStatus{}, err
	}
	_, key, err := s.credentialFor(ctx, principalID, p.server)
	if err != nil {
		return ServerStatus{}, err
	}
	status := ServerStatus{
		Name:        p.server.Name,
		Title:       p.server.Title,
		Description: p.server.Description,
		Keyed:       p.server.Auth == config.MCPAuthKey,
		Enabled:     enabled,
		Key:         key,
	}
	switch {
	case !enabled:
		status.State = StateOff
	case key == KeyMissing:
		status.State = StateNeedsKey
	case key == KeyStale:
		status.State = StateNeedsReconnect
	case key == KeyRefused:
		status.State = StateProblem
	default:
		status.State = StateOn
	}
	return status, nil
}

// SetEnabled turns the server on or off for the principal.
func (s *Service) SetEnabled(ctx context.Context, principalID, server string, enabled bool) (ServerStatus, error) {
	p, err := s.allowed(principalID, server)
	if err != nil {
		return ServerStatus{}, err
	}
	if err := s.switches.SetEnabled(ctx, principalID, p.sw, enabled); err != nil {
		return ServerStatus{}, err
	}
	return s.status(ctx, principalID, p)
}

func (s *Service) allowed(principalID, server string) (*provider, error) {
	p, known := s.byName[server]
	if !known {
		return nil, ErrUnknownServer
	}
	if !p.server.Allows(principalID) {
		return nil, ErrNotAllowed
	}
	return p, nil
}

func (s *Service) keyed(principalID, server string) (*provider, error) {
	p, err := s.allowed(principalID, server)
	if err != nil {
		return nil, err
	}
	if p.server.Auth != config.MCPAuthKey {
		return nil, ErrNoKeyNeeded
	}
	return p, nil
}

// ValidKey reports whether text can be a key for the server: a single token of 16 to 512
// characters without whitespace or control characters that matches the entry's key pattern. An
// absolute URL is never a key; it is more likely another service's sign-in address.
func ValidKey(server config.MCPServer, text string) bool {
	if strings.Contains(text, "://") {
		return false
	}
	length := 0
	for _, r := range text {
		length++
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	if length < minimumKeyLength || length > maximumKeyLength {
		return false
	}
	return server.KeyPattern == nil || server.KeyPattern.MatchString(text)
}

// MatchingKeyServers names the keyed servers the principal may use whose key pattern the text
// matches as a key, in catalog order. A server without a key pattern matches nothing, since any
// token could be its key.
func (s *Service) MatchingKeyServers(principalID, text string) []string {
	var names []string
	for _, p := range s.providers {
		if p.server.Auth == config.MCPAuthKey && p.server.KeyPattern != nil && p.server.Allows(principalID) && ValidKey(p.server, text) {
			names = append(names, p.server.Name)
		}
	}
	return names
}

// Connect verifies the principal's key with initialize and tools/list, stores it, records the
// connection bound to the server's origin, retires the session of any previous key, and turns the
// server on. A key the server refuses with 401 or 403 is ErrKeyRefused; a server that fails
// otherwise is ErrKeyUnverified; in both cases nothing changes. When the record cannot be saved the
// previous key file is put back. When the key is stored but the switch cannot be turned on, the
// status is returned with ErrSwitchUnchanged.
func (s *Service) Connect(ctx context.Context, principalID, server, key string) (ServerStatus, error) {
	p, err := s.keyed(principalID, server)
	if err != nil {
		return ServerStatus{}, err
	}
	if !ValidKey(p.server, key) {
		return ServerStatus{}, ErrInvalidKey
	}
	if err := s.verify(ctx, p.server, key); err != nil {
		switch {
		case errors.Is(err, ErrUnauthorized):
			s.logger.InfoContext(ctx, "mcp.connection.rejected", "principal_id", principalID, "server", server)
			return ServerStatus{}, ErrKeyRefused
		case ctx.Err() != nil:
			return ServerStatus{}, ctx.Err()
		default:
			s.logger.WarnContext(ctx, "mcp.connection.verify_failed", "principal_id", principalID, "server", server, "class", ClassOf(err))
			return ServerStatus{}, ErrKeyUnverified
		}
	}
	name := IntegrationName(server)
	previous, hadPrevious, readErr := s.credentials.Read(principalID, name)
	if err := s.credentials.Write(principalID, name, key); err != nil {
		return ServerStatus{}, err
	}
	record := integration.Connection{
		PrincipalID:   principalID,
		Integration:   name,
		TokenKind:     integration.TokenKindPersonal,
		WorkspaceID:   p.server.Origin(),
		WorkspaceName: clip.Runes(p.server.Title, maximumTitleLen),
	}
	if err := s.repository.SaveIntegrationConnection(ctx, record); err != nil {
		if hadPrevious && readErr == nil {
			_ = s.credentials.Write(principalID, name, previous)
		} else {
			_, _ = s.credentials.Remove(principalID, name)
		}
		return ServerStatus{}, err
	}
	s.forget(principalID, p.server)
	s.logger.InfoContext(ctx, "mcp.connection.connected", "principal_id", principalID, "server", server)
	if err := s.switches.SetEnabled(ctx, principalID, p.sw, true); err != nil {
		s.logger.WarnContext(ctx, "mcp.connection.switch_failed", "principal_id", principalID, "server", server)
		status, _ := s.status(ctx, principalID, p)
		return status, ErrSwitchUnchanged
	}
	return s.status(ctx, principalID, p)
}

func (s *Service) verify(ctx context.Context, server config.MCPServer, key string) error {
	client, err := newClient(server, key)
	if err != nil {
		return ErrInvalidOptions
	}
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancelClose()
		client.Close(closeCtx)
	}()
	if err := client.Initialize(verifyCtx); err != nil {
		return err
	}
	_, err = client.ListTools(verifyCtx)
	return err
}

// Disconnect retires the principal's session and cached list first, so nothing usable stays in
// memory even when a later step fails, then removes the key file and the connection record, and
// turns the server off. It waits for calls in flight on the session to end. A pending intake is
// cancelled only when it is for this server. It reports whether there was a key or record to
// remove.
func (s *Service) Disconnect(ctx context.Context, principalID, server string) (bool, error) {
	p, err := s.keyed(principalID, server)
	if err != nil {
		return false, err
	}
	s.forget(principalID, p.server)
	name := IntegrationName(server)
	removed, err := s.credentials.Remove(principalID, name)
	if err != nil {
		return false, err
	}
	deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, name)
	if err != nil {
		return false, err
	}
	// A turn that read the key between the first forget and its removal may have opened a session.
	s.forget(principalID, p.server)
	s.mu.Lock()
	if s.intakes[principalID].server == server {
		delete(s.intakes, principalID)
	}
	s.mu.Unlock()
	if err := s.switches.SetEnabled(ctx, principalID, p.sw, false); err != nil {
		return false, err
	}
	if removed || deleted {
		s.logger.InfoContext(ctx, "mcp.connection.disconnected", "principal_id", principalID, "server", server)
	}
	return removed || deleted, nil
}

// BeginIntake takes the principal's next message as a key for the server, replacing any intake
// the principal had pending. It returns when the intake expires.
func (s *Service) BeginIntake(principalID, server string) (time.Time, error) {
	if _, err := s.keyed(principalID, server); err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires := s.now().Add(s.intakeLifetime)
	s.intakes[principalID] = pendingIntake{server: server, expires: expires}
	return expires, nil
}

// PendingIntake names the server the principal's unexpired intake is for.
func (s *Service) PendingIntake(principalID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, found := s.intakes[principalID]
	if !found || !s.now().Before(pending.expires) {
		return "", false
	}
	return pending.server, true
}

// CancelIntake ends the principal's intake and reports whether one was pending, expired or not.
func (s *Service) CancelIntake(principalID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.intakes[principalID]
	delete(s.intakes, principalID)
	return found
}

// IntakeOutcome is what became of a message offered to the key intake.
type IntakeOutcome string

const (
	// IntakeNone means the message is an ordinary one: no intake was pending, one had expired and
	// the message is not a key for its server, or one had expired longer ago than its lifetime.
	IntakeNone IntakeOutcome = "none"
	// IntakeExpired means the intake had expired, less than its lifetime ago, and the message is a
	// key for its server, pasted late; nothing was sent and the intake is ended.
	IntakeExpired IntakeOutcome = "expired"
	// IntakeNotKey means the message did not look like a key; the intake is ended and the message
	// was sent nowhere.
	IntakeNotKey IntakeOutcome = "not_key"
	// IntakeConnected means the key was verified and stored and the intake is ended. Status shows
	// the server; it stays off when the switch could not be turned on.
	IntakeConnected IntakeOutcome = "connected"
	// IntakeRefused means the server refused the key; nothing was stored and the intake stays
	// pending until it expires, so a key pasted again is still taken.
	IntakeRefused IntakeOutcome = "refused"
	// IntakeUnverified means the server failed before it could judge the key; nothing was stored
	// and the intake stays pending until it expires.
	IntakeUnverified IntakeOutcome = "unverified"
	// IntakeFailed means the server accepted the key but storing it failed; OfferMessage returns
	// the error with it and the intake stays pending until it expires.
	IntakeFailed IntakeOutcome = "failed"
)

// IntakeResult is the outcome of an offered message, the server the intake was for, and, once
// connected, the server's status.
type IntakeResult struct {
	Outcome IntakeOutcome
	Server  string
	Status  ServerStatus
}

// OfferMessage gives the principal's message to their pending intake. Any outcome other than
// IntakeNone consumes the message: the caller deletes it from the chat and never passes it on.
// An error comes only with IntakeFailed. A message that ended an intake is remembered for the
// intake lifetime, so the same message offered again while no intake is pending, such as a
// retried update, gets the same result instead of passing on as an ordinary message.
func (s *Service) OfferMessage(ctx context.Context, principalID, text string) (IntakeResult, error) {
	mark := s.mark(principalID, text)
	s.mu.Lock()
	now := s.now()
	s.sweepConsumedLocked(now)
	pending, found := s.intakes[principalID]
	live := found && now.Before(pending.expires)
	if !live {
		for _, seen := range s.consumed[principalID] {
			if hmac.Equal([]byte(seen.mark), []byte(mark)) {
				s.mu.Unlock()
				return seen.result, nil
			}
		}
	}
	if found && !live {
		delete(s.intakes, principalID)
	}
	s.mu.Unlock()
	// An intake abandoned for longer than its lifetime takes nothing, however the message looks.
	if !found || !now.Before(pending.expires.Add(s.intakeLifetime)) {
		return IntakeResult{Outcome: IntakeNone}, nil
	}
	p, err := s.keyed(principalID, pending.server)
	valid := err == nil && ValidKey(p.server, text)
	result := IntakeResult{Server: pending.server}
	switch {
	case !live && valid:
		result.Outcome = IntakeExpired
		s.remember(principalID, mark, result)
		return result, nil
	case !live:
		return IntakeResult{Outcome: IntakeNone}, nil
	case !valid:
		s.endIntake(principalID, pending)
		result.Outcome = IntakeNotKey
		s.remember(principalID, mark, result)
		return result, nil
	}
	status, err := s.Connect(ctx, principalID, pending.server, text)
	switch {
	case err == nil, errors.Is(err, ErrSwitchUnchanged):
		s.endIntake(principalID, pending)
		result.Outcome, result.Status = IntakeConnected, status
		s.remember(principalID, mark, result)
		return result, nil
	case errors.Is(err, ErrKeyRefused):
		result.Outcome = IntakeRefused
		return result, nil
	case errors.Is(err, ErrKeyUnverified):
		result.Outcome = IntakeUnverified
		return result, nil
	default:
		result.Outcome = IntakeFailed
		return result, err
	}
}

// mark is the message's keyed fingerprint; the text itself is never kept.
func (s *Service) mark(principalID, text string) string {
	mac := hmac.New(sha256.New, s.consumeSecret)
	mac.Write([]byte(principalID))
	mac.Write([]byte{0})
	mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

// remember keeps the result of a message that ended an intake, dropping the principal's oldest
// beyond the bound. A result that leaves the intake pending needs no memory: the intake takes the
// message again.
func (s *Service) remember(principalID, mark string, result IntakeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := append(s.consumed[principalID], consumedMessage{mark: mark, at: s.now(), result: result})
	if len(kept) > consumedPerPrincipal {
		kept = slices.Clone(kept[len(kept)-consumedPerPrincipal:])
	}
	s.consumed[principalID] = kept
}

// sweepConsumedLocked forgets messages consumed longer than the intake lifetime ago; the caller
// holds s.mu.
func (s *Service) sweepConsumedLocked(now time.Time) {
	for principalID, kept := range s.consumed {
		kept = slices.DeleteFunc(kept, func(seen consumedMessage) bool { return now.Sub(seen.at) >= s.intakeLifetime })
		if len(kept) == 0 {
			delete(s.consumed, principalID)
			continue
		}
		s.consumed[principalID] = kept
	}
}

// endIntake ends the intake unless another one replaced it meanwhile.
func (s *Service) endIntake(principalID string, pending pendingIntake) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intakes[principalID] == pending {
		delete(s.intakes, principalID)
	}
}
