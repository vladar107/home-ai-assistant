package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
)

const (
	maxMCPServers               = 12
	maxMCPToolsPerServer        = 16
	maxMCPTitleLength           = 48
	maxMCPDescriptionLength     = 160
	maxMCPGuidanceLength        = 300
	maxMCPToolNameLength        = 128
	maxMCPToolDescriptionLength = 1024
	maxMCPKeyPrefixLength       = 32
	maxMCPKeyPatternLength      = 256
	// maxMCPOriginLength matches the integration_connections workspace column the origin of a
	// keyed server is recorded in.
	maxMCPOriginLength = 128

	defaultMCPKeyHeader = "Authorization"
	defaultMCPKeyPrefix = "Bearer "
)

var mcpServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// mcpReservedHeaders are the headers a key must never be sent in: hop-by-hop headers a proxy
// strips or acts on, and Host. Content-* headers are refused by prefix.
var mcpReservedHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"host":                {},
	// The client sets these itself on every request.
	"accept":               {},
	"mcp-session-id":       {},
	"mcp-protocol-version": {},
}

type MCPAuth string

const (
	MCPAuthNone MCPAuth = "none"
	MCPAuthKey  MCPAuth = "key"
)

// MCPConfig is the operator's catalog of MCP servers. An empty catalog means the feature is off.
type MCPConfig struct {
	ServersFile string
	Servers     []MCPServer
}

func (c MCPConfig) Enabled() bool {
	return len(c.Servers) > 0
}

// MCPServer is one catalog entry. KeyHeader, KeyPrefix, and KeyPattern apply only to a key
// server; KeyPattern is nil when the entry names none. Principals is nil when everyone may use
// the server.
type MCPServer struct {
	Name        string
	Title       string
	Description string
	URL         string
	Auth        MCPAuth
	KeyHeader   string
	KeyPrefix   string
	KeyPattern  *regexp.Regexp
	Tools       []MCPTool
	Guidance    string
	Principals  []string
}

// MCPTool is one allowlisted remote tool. Description is empty unless the entry pins one.
type MCPTool struct {
	Name        string
	Description string
}

// Origin returns scheme://host[:port] of the server's URL, with the host in lower case.
func (s MCPServer) Origin() string {
	parsed, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Host)
}

// Allows reports whether the principal may use the server.
func (s MCPServer) Allows(principalID string) bool {
	if s.Principals == nil {
		return true
	}
	for _, principal := range s.Principals {
		if principal == principalID {
			return true
		}
	}
	return false
}

type mcpServersFile struct {
	Servers []mcpServerEntry `json:"servers"`
}

// mcpServerEntry keeps the header and prefix as pointers so an explicit empty prefix (a bare key)
// is told apart from an absent one, which takes the default.
type mcpServerEntry struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	URL         string         `json:"url"`
	Auth        MCPAuth        `json:"auth"`
	KeyHeader   *string        `json:"key_header"`
	KeyPrefix   *string        `json:"key_prefix"`
	KeyPattern  *string        `json:"key_pattern"`
	Tools       []mcpToolEntry `json:"tools"`
	Guidance    string         `json:"guidance"`
	Principals  []string       `json:"principals"`
}

type mcpToolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func loadMCPConfig(cfg *MCPConfig) error {
	cfg.ServersFile = strings.TrimSpace(os.Getenv("HOME_AGENT_MCP_SERVERS_FILE"))
	if cfg.ServersFile == "" {
		return nil
	}
	servers, err := loadMCPServers(cfg.ServersFile)
	if err != nil {
		return err
	}
	cfg.Servers = servers
	return nil
}

func loadMCPServers(path string) ([]MCPServer, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: cannot open configured file")
	}
	defer file.Close()

	var document mcpServersFile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: invalid trailing content")
	}
	if len(document.Servers) == 0 {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: must list at least one server")
	}
	if len(document.Servers) > maxMCPServers {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: lists more than %d servers", maxMCPServers)
	}
	servers := make([]MCPServer, 0, len(document.Servers))
	names := make(map[string]struct{}, len(document.Servers))
	for _, entry := range document.Servers {
		server, err := entry.server()
		if err != nil {
			return nil, err
		}
		if _, duplicate := names[server.Name]; duplicate {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q appears twice", server.Name)
		}
		names[server.Name] = struct{}{}
		servers = append(servers, server)
	}
	return servers, nil
}

func (e mcpServerEntry) server() (MCPServer, error) {
	if !mcpServerNamePattern.MatchString(e.Name) {
		return MCPServer{}, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q is not a server name", e.Name)
	}
	server := MCPServer{
		Name:        e.Name,
		Title:       strings.TrimSpace(e.Title),
		Description: strings.TrimSpace(e.Description),
		URL:         strings.TrimSpace(e.URL),
		Auth:        e.Auth,
		Guidance:    strings.TrimSpace(e.Guidance),
	}
	if err := validateMCPLine(server.Name, "title", server.Title, maxMCPTitleLength, true); err != nil {
		return MCPServer{}, err
	}
	if err := validateMCPLine(server.Name, "description", server.Description, maxMCPDescriptionLength, true); err != nil {
		return MCPServer{}, err
	}
	if err := validateMCPLine(server.Name, "guidance", server.Guidance, maxMCPGuidanceLength, false); err != nil {
		return MCPServer{}, err
	}
	carriesKeys, err := validateMCPURL(server.Name, server.URL)
	if err != nil {
		return MCPServer{}, err
	}
	if err := e.applyAuth(&server, carriesKeys); err != nil {
		return MCPServer{}, err
	}
	tools, err := e.tools()
	if err != nil {
		return MCPServer{}, err
	}
	server.Tools = tools
	principals, err := e.principals()
	if err != nil {
		return MCPServer{}, err
	}
	server.Principals = principals
	return server, nil
}

func validateMCPLine(server, field, value string, maxLength int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q needs a %s", server, field)
		}
		return nil
	}
	if utf8.RuneCountInString(value) > maxLength {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q %s is longer than %d characters", server, field, maxLength)
	}
	if !singleLine(value) {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q %s must be a single line", server, field)
	}
	return nil
}

func singleLine(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// validateMCPURL reports whether the URL may carry a user's key: over HTTPS, or to a loopback host.
// It never echoes the URL, since a mistyped one may hold credentials.
func validateMCPURL(server, raw string) (carriesKeys bool, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url must be an absolute http or https URL", server)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url must be an absolute http or https URL", server)
	}
	if parsed.Hostname() == "" {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url has no host", server)
	}
	if parsed.User != nil {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url must not carry user info", server)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "?") {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url must not carry a query", server)
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url must not carry a fragment", server)
	}
	if len(parsed.Scheme+"://"+parsed.Host) > maxMCPOriginLength {
		return false, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q url origin is longer than %d characters", server, maxMCPOriginLength)
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	return parsed.Scheme == "https" || loopback, nil
}

func (e mcpServerEntry) applyAuth(server *MCPServer, carriesKeys bool) error {
	switch e.Auth {
	case MCPAuthNone:
		if e.KeyHeader != nil || e.KeyPrefix != nil || e.KeyPattern != nil {
			return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q sets key_header, key_prefix, or key_pattern without auth key", server.Name)
		}
		return nil
	case MCPAuthKey:
	default:
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q auth must be none or key", server.Name)
	}
	if !carriesKeys {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q uses auth key and must use https unless its host is loopback", server.Name)
	}
	server.KeyHeader = defaultMCPKeyHeader
	if e.KeyHeader != nil {
		server.KeyHeader = *e.KeyHeader
	}
	if err := validateMCPKeyHeader(server.Name, server.KeyHeader); err != nil {
		return err
	}
	server.KeyPrefix = defaultMCPKeyPrefix
	if e.KeyPrefix != nil {
		server.KeyPrefix = *e.KeyPrefix
	}
	if utf8.RuneCountInString(server.KeyPrefix) > maxMCPKeyPrefixLength || !httpguts.ValidHeaderFieldValue(server.KeyPrefix) {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q key_prefix must be at most %d characters with no line breaks or control characters", server.Name, maxMCPKeyPrefixLength)
	}
	if e.KeyPattern != nil {
		if *e.KeyPattern == "" || len(*e.KeyPattern) > maxMCPKeyPatternLength {
			return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q key_pattern must be 1 to %d characters", server.Name, maxMCPKeyPatternLength)
		}
		// Anchored so the pattern describes the whole key, not some part of the message.
		pattern, err := regexp.Compile(`^(?:` + *e.KeyPattern + `)$`)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q key_pattern does not compile", server.Name)
		}
		server.KeyPattern = pattern
	}
	return nil
}

func validateMCPKeyHeader(server, header string) error {
	if !httpguts.ValidHeaderFieldName(header) {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q key_header is not a valid header name", server)
	}
	lower := strings.ToLower(header)
	if _, reserved := mcpReservedHeaders[lower]; reserved || strings.HasPrefix(lower, "content-") {
		return fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q key_header must not be a hop-by-hop, Host, Content-*, or MCP transport header", server)
	}
	return nil
}

func (e mcpServerEntry) tools() ([]MCPTool, error) {
	if len(e.Tools) == 0 {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q allowlists no tools", e.Name)
	}
	if len(e.Tools) > maxMCPToolsPerServer {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q allowlists more than %d tools", e.Name, maxMCPToolsPerServer)
	}
	tools := make([]MCPTool, 0, len(e.Tools))
	names := make(map[string]struct{}, len(e.Tools))
	for _, entry := range e.Tools {
		if entry.Name == "" || utf8.RuneCountInString(entry.Name) > maxMCPToolNameLength || strings.IndexFunc(entry.Name, unicode.IsSpace) >= 0 || !singleLine(entry.Name) {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q has a tool name that is empty, longer than %d characters, or contains whitespace", e.Name, maxMCPToolNameLength)
		}
		if _, duplicate := names[entry.Name]; duplicate {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q allowlists %q twice", e.Name, entry.Name)
		}
		names[entry.Name] = struct{}{}
		tool := MCPTool{Name: entry.Name, Description: strings.TrimSpace(entry.Description)}
		if entry.Description != "" && tool.Description == "" {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q tool %q has a blank description", e.Name, entry.Name)
		}
		if utf8.RuneCountInString(tool.Description) > maxMCPToolDescriptionLength {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q tool %q description is longer than %d characters", e.Name, entry.Name, maxMCPToolDescriptionLength)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (e mcpServerEntry) principals() ([]string, error) {
	if e.Principals == nil {
		return nil, nil
	}
	if len(e.Principals) == 0 {
		return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q lists no principals; omit principals to allow everyone", e.Name)
	}
	seen := make(map[string]struct{}, len(e.Principals))
	principals := make([]string, 0, len(e.Principals))
	for _, principal := range e.Principals {
		principal = strings.TrimSpace(principal)
		if !nasPrincipalPattern.MatchString(principal) {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q lists %q, which is not a principal ID", e.Name, principal)
		}
		if _, duplicate := seen[principal]; duplicate {
			return nil, fmt.Errorf("HOME_AGENT_MCP_SERVERS_FILE: %q lists principal %q twice", e.Name, principal)
		}
		seen[principal] = struct{}{}
		principals = append(principals, principal)
	}
	return principals, nil
}
