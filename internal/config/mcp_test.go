package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validMCPServer(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"title":       "Listings",
		"description": "Search classified listings.",
		"url":         "https://mcp.example.com/mcp",
		"auth":        "none",
		"tools":       []any{map[string]any{"name": "search"}},
	}
}

func withMCP(server map[string]any, overrides map[string]any) map[string]any {
	for key, value := range overrides {
		if value == nil {
			delete(server, key)
			continue
		}
		server[key] = value
	}
	return server
}

func writeMCPCatalog(t *testing.T, document string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "mcp-servers.json")
	if err := os.WriteFile(file, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func loadMCPServersFrom(t *testing.T, servers ...map[string]any) ([]MCPServer, error) {
	t.Helper()
	document, err := json.Marshal(map[string]any{"servers": servers})
	if err != nil {
		t.Fatal(err)
	}
	return loadMCPServers(writeMCPCatalog(t, string(document)))
}

type mcpCase struct {
	name      string
	overrides map[string]any
	want      string
}

func runMCPCases(t *testing.T, cases []mcpCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadMCPServersFrom(t, withMCP(validMCPServer("listings"), test.overrides))
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if !strings.HasPrefix(err.Error(), "HOME_AGENT_MCP_SERVERS_FILE: ") {
				t.Fatalf("error %q does not name the variable", err)
			}
		})
	}
}

func TestMCPCatalogUnsetLeavesFeatureOff(t *testing.T) {
	for _, value := range []string{"", "  "} {
		t.Setenv("HOME_AGENT_MCP_SERVERS_FILE", value)
		var cfg MCPConfig
		if err := loadMCPConfig(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Enabled() || cfg.Servers != nil {
			t.Fatalf("feature on with %q: %+v", value, cfg)
		}
	}
}

func TestMCPCatalogFailureFailsConfigLoad(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{"invalid JSON", `{"servers":[`, "invalid JSON"},
		{"unknown field", `{"servers":[],"extra":true}`, "invalid JSON"},
		{"unknown server field", `{"servers":[{"name":"a","command":"run"}]}`, "invalid JSON"},
		{"trailing content", `{"servers":[]} {}`, "invalid trailing content"},
		{"no servers", `{"servers":[]}`, "at least one server"},
		{"invalid entry", `{"servers":[{"name":"A"}]}`, "not a server name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME_AGENT_MCP_SERVERS_FILE", writeMCPCatalog(t, test.document))
			var cfg MCPConfig
			err := loadMCPConfig(&cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("HOME_AGENT_MCP_SERVERS_FILE", filepath.Join(t.TempDir(), "absent.json"))
		var cfg MCPConfig
		if err := loadMCPConfig(&cfg); err == nil || !strings.Contains(err.Error(), "cannot open") {
			t.Fatalf("error = %v, want cannot open", err)
		}
	})
}

func TestMCPExampleCatalogLoads(t *testing.T) {
	t.Setenv("HOME_AGENT_MCP_SERVERS_FILE", filepath.Join("..", "..", "deploy", "mcp-servers.example.json"))
	var cfg MCPConfig
	if err := loadMCPConfig(&cfg); err != nil {
		t.Fatalf("example catalog: %v", err)
	}
	if !cfg.Enabled() || len(cfg.Servers) != 2 {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
	shared, keyed := cfg.Servers[0], cfg.Servers[1]
	if shared.Auth != MCPAuthNone || shared.Origin() != "http://example-sidecar:8000" || shared.Principals != nil || shared.KeyHeader != "" {
		t.Fatalf("shared server = %+v", shared)
	}
	if shared.Tools[1].Description == "" || shared.Tools[0].Description != "" {
		t.Fatalf("shared tools = %+v", shared.Tools)
	}
	if keyed.Auth != MCPAuthKey || keyed.Origin() != "https://mcp.example.com" || keyed.KeyHeader != "Authorization" || keyed.KeyPrefix != "Bearer " {
		t.Fatalf("keyed server = %+v", keyed)
	}
	if !keyed.Allows("person-one") || keyed.Allows("person-three") || !shared.Allows("person-three") {
		t.Fatal("principal list not applied")
	}
}

func TestMCPServerName(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"lower case letters and digits", map[string]any{"name": "shop2"}, ""},
		{"sixteen characters", map[string]any{"name": "abcdefghijklmnop"}, ""},
		{"seventeen characters", map[string]any{"name": "abcdefghijklmnopq"}, "not a server name"},
		{"upper case", map[string]any{"name": "Shop"}, "not a server name"},
		{"leading digit", map[string]any{"name": "2shop"}, "not a server name"},
		{"underscore", map[string]any{"name": "my_shop"}, "not a server name"},
		{"empty", map[string]any{"name": ""}, "not a server name"},
	})
	t.Run("unique", func(t *testing.T) {
		_, err := loadMCPServersFrom(t, validMCPServer("shop"), validMCPServer("shop"))
		if err == nil || !strings.Contains(err.Error(), "appears twice") {
			t.Fatalf("error = %v, want appears twice", err)
		}
	})
}

func TestMCPServerCount(t *testing.T) {
	servers := func(count int) []map[string]any {
		list := make([]map[string]any, 0, count)
		for index := range count {
			list = append(list, validMCPServer(fmt.Sprintf("s%d", index)))
		}
		return list
	}
	if _, err := loadMCPServersFrom(t, servers(12)...); err != nil {
		t.Fatalf("twelve servers: %v", err)
	}
	if _, err := loadMCPServersFrom(t, servers(13)...); err == nil || !strings.Contains(err.Error(), "more than 12 servers") {
		t.Fatalf("error = %v, want more than 12 servers", err)
	}
}

func TestMCPServerTools(t *testing.T) {
	tools := func(count int) []any {
		list := make([]any, 0, count)
		for index := range count {
			list = append(list, map[string]any{"name": fmt.Sprintf("tool_%d", index)})
		}
		return list
	}
	runMCPCases(t, []mcpCase{
		{"sixteen tools", map[string]any{"tools": tools(16)}, ""},
		{"seventeen tools", map[string]any{"tools": tools(17)}, "more than 16 tools"},
		{"no tools", map[string]any{"tools": []any{}}, "allowlists no tools"},
		{"tools absent", map[string]any{"tools": nil}, "allowlists no tools"},
		{"empty tool name", map[string]any{"tools": []any{map[string]any{"name": ""}}}, "tool name"},
		{"tool name with space", map[string]any{"tools": []any{map[string]any{"name": "find items"}}}, "tool name"},
		{"duplicate tool", map[string]any{"tools": []any{map[string]any{"name": "search"}, map[string]any{"name": "search"}}}, `"search" twice`},
		{"pinned description", map[string]any{"tools": []any{map[string]any{"name": "search", "description": "Find listings."}}}, ""},
		{"blank pinned description", map[string]any{"tools": []any{map[string]any{"name": "search", "description": "  "}}}, "blank description"},
		{"long pinned description", map[string]any{"tools": []any{map[string]any{"name": "search", "description": strings.Repeat("a", 1025)}}}, "longer than 1024"},
	})
}

func TestMCPTitleAndDescription(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"title at bound", map[string]any{"title": strings.Repeat("t", 48)}, ""},
		{"title too long", map[string]any{"title": strings.Repeat("t", 49)}, "title is longer than 48"},
		{"title missing", map[string]any{"title": nil}, "needs a title"},
		{"title blank", map[string]any{"title": "   "}, "needs a title"},
		{"title two lines", map[string]any{"title": "Listings\nSearch"}, "title must be a single line"},
		{"description at bound", map[string]any{"description": strings.Repeat("d", 160)}, ""},
		{"description too long", map[string]any{"description": strings.Repeat("d", 161)}, "description is longer than 160"},
		{"description missing", map[string]any{"description": nil}, "needs a description"},
		{"description two lines", map[string]any{"description": "Search.\r\nThen read."}, "description must be a single line"},
		{"description line separator", map[string]any{"description": "Search.\u2028Then read."}, "description must be a single line"},
	})
}

func TestMCPGuidance(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"absent", map[string]any{"guidance": nil}, ""},
		{"one line", map[string]any{"guidance": "Use for second-hand items."}, ""},
		{"too long", map[string]any{"guidance": strings.Repeat("g", 301)}, "guidance is longer than 300"},
		{"two lines", map[string]any{"guidance": "Use it.\nAlways."}, "guidance must be a single line"},
	})
}

func TestMCPURL(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"https with path", map[string]any{"url": "https://mcp.example.com/v1/mcp"}, ""},
		{"http container host and port", map[string]any{"url": "http://example-sidecar:8000/mcp"}, ""},
		{"relative", map[string]any{"url": "/mcp"}, "absolute http or https"},
		{"other scheme", map[string]any{"url": "ftp://mcp.example.com/mcp"}, "absolute http or https"},
		{"opaque", map[string]any{"url": "https:mcp.example.com"}, "absolute http or https"},
		{"no host", map[string]any{"url": "https:///mcp"}, "has no host"},
		{"missing", map[string]any{"url": nil}, "absolute http or https"},
		{"user info", map[string]any{"url": "https://user:secret@mcp.example.com/mcp"}, "user info"},
		{"query", map[string]any{"url": "https://mcp.example.com/mcp?key=secret"}, "query"},
		{"empty query", map[string]any{"url": "https://mcp.example.com/mcp?"}, "query"},
		{"fragment", map[string]any{"url": "https://mcp.example.com/mcp#top"}, "fragment"},
		{"empty fragment", map[string]any{"url": "https://mcp.example.com/mcp#"}, "fragment"},
	})
	t.Run("error never echoes the URL", func(t *testing.T) {
		_, err := loadMCPServersFrom(t, withMCP(validMCPServer("listings"), map[string]any{"url": "https://user:secret@mcp.example.com/mcp"}))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestMCPOriginLength(t *testing.T) {
	host := func(originLength int) string {
		return strings.Repeat("a", originLength-len("https://")-len(".example"))
	}
	runMCPCases(t, []mcpCase{
		{"origin of 128 characters", map[string]any{"url": "https://" + host(128) + ".example/mcp"}, ""},
		{"origin of 129 characters", map[string]any{"url": "https://" + host(129) + ".example/mcp"}, "origin is longer than 128"},
	})
	server := MCPServer{URL: "HTTPS://MCP.Example.com:8443/path/mcp"}
	if got := server.Origin(); got != "https://mcp.example.com:8443" {
		t.Fatalf("origin = %q", got)
	}
}

func TestMCPAuthKind(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"none", map[string]any{"auth": "none"}, ""},
		{"key", map[string]any{"auth": "key"}, ""},
		{"missing", map[string]any{"auth": nil}, "auth must be none or key"},
		{"upper case", map[string]any{"auth": "Key"}, "auth must be none or key"},
		{"oauth", map[string]any{"auth": "oauth"}, "auth must be none or key"},
		{"key_header on none", map[string]any{"key_header": "X-Api-Key"}, "without auth key"},
		{"key_prefix on none", map[string]any{"key_prefix": ""}, "without auth key"},
		{"key_pattern on none", map[string]any{"key_pattern": "[a-z]+"}, "without auth key"},
	})
}

func TestMCPKeyServerNeedsHTTPSOrLoopback(t *testing.T) {
	keyed := func(url string) map[string]any { return map[string]any{"auth": "key", "url": url} }
	runMCPCases(t, []mcpCase{
		{"https", keyed("https://mcp.example.com/mcp"), ""},
		{"http container host", keyed("http://example-sidecar:8000/mcp"), "must use https"},
		{"http private address", keyed("http://192.0.2.10:8000/mcp"), "must use https"},
		{"http localhost", keyed("http://localhost:8000/mcp"), ""},
		{"http LOCALHOST", keyed("http://LOCALHOST:8000/mcp"), ""},
		{"http 127.0.0.1", keyed("http://127.0.0.1:8000/mcp"), ""},
		{"http elsewhere in 127/8", keyed("http://127.1.2.3:8000/mcp"), ""},
		{"http ::1", keyed("http://[::1]:8000/mcp"), ""},
		{"http none server", map[string]any{"auth": "none", "url": "http://example-sidecar:8000/mcp"}, ""},
	})
}

func TestMCPKeyHeaderAndPrefix(t *testing.T) {
	keyed := func(overrides map[string]any) map[string]any {
		overrides["auth"] = "key"
		return overrides
	}
	cases := []mcpCase{
		{"custom header", keyed(map[string]any{"key_header": "X-Api-Key", "key_prefix": ""}), ""},
		{"header with line break", keyed(map[string]any{"key_header": "X-Api-Key\r\nX-Other"}), "not a valid header name"},
		{"header with space", keyed(map[string]any{"key_header": "Api Key"}), "not a valid header name"},
		{"empty header", keyed(map[string]any{"key_header": ""}), "not a valid header name"},
		{"prefix with line break", keyed(map[string]any{"key_prefix": "Bearer\r\n"}), "no line breaks"},
		{"prefix with newline", keyed(map[string]any{"key_prefix": "Token\n"}), "no line breaks"},
		{"prefix too long", keyed(map[string]any{"key_prefix": strings.Repeat("p", 33)}), "at most 32"},
		{"Host", keyed(map[string]any{"key_header": "Host"}), "hop-by-hop, Host, Content-*, or MCP transport"},
		{"Content-Type", keyed(map[string]any{"key_header": "Content-Type"}), "hop-by-hop, Host, Content-*, or MCP transport"},
		{"Accept", keyed(map[string]any{"key_header": "Accept"}), "hop-by-hop, Host, Content-*, or MCP transport"},
		{"session header", keyed(map[string]any{"key_header": "MCP-Session-Id"}), "hop-by-hop, Host, Content-*, or MCP transport"},
		{"version header", keyed(map[string]any{"key_header": "Mcp-Protocol-Version"}), "hop-by-hop, Host, Content-*, or MCP transport"},
		{"content-length in lower case", keyed(map[string]any{"key_header": "content-length"}), "hop-by-hop, Host, Content-*, or MCP transport"},
	}
	for _, header := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "proxy-authorization", "TE", "Trailer", "Transfer-Encoding", "UPGRADE"} {
		cases = append(cases, mcpCase{"hop-by-hop " + header, keyed(map[string]any{"key_header": header}), "hop-by-hop, Host, Content-*, or MCP transport"})
	}
	runMCPCases(t, cases)

	servers, err := loadMCPServersFrom(t,
		withMCP(validMCPServer("defaults"), map[string]any{"auth": "key"}),
		withMCP(validMCPServer("bare"), map[string]any{"auth": "key", "key_header": "X-Api-Key", "key_prefix": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].KeyHeader != "Authorization" || servers[0].KeyPrefix != "Bearer " {
		t.Fatalf("defaults = %q %q", servers[0].KeyHeader, servers[0].KeyPrefix)
	}
	if servers[1].KeyHeader != "X-Api-Key" || servers[1].KeyPrefix != "" {
		t.Fatalf("explicit = %q %q", servers[1].KeyHeader, servers[1].KeyPrefix)
	}
}

func TestMCPKeyPattern(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"compiles", map[string]any{"auth": "key", "key_pattern": "[a-f0-9]{32}"}, ""},
		{"does not compile", map[string]any{"auth": "key", "key_pattern": "([a-f"}, "key_pattern does not compile"},
		{"empty", map[string]any{"auth": "key", "key_pattern": ""}, "key_pattern must be 1 to 256"},
	})
	servers, err := loadMCPServersFrom(t, withMCP(validMCPServer("prices"), map[string]any{"auth": "key", "key_pattern": "[a-f0-9]{4}|key-[0-9]+"}))
	if err != nil {
		t.Fatal(err)
	}
	pattern := servers[0].KeyPattern
	for key, want := range map[string]bool{"ab12": true, "key-7": true, "ab12x": false, "xkey-7": false, "": false} {
		if got := pattern.MatchString(key); got != want {
			t.Fatalf("pattern matches %q = %v, want %v", key, got, want)
		}
	}
}

func TestMCPPrincipals(t *testing.T) {
	runMCPCases(t, []mcpCase{
		{"listed", map[string]any{"principals": []string{"person-one", "person.two"}}, ""},
		{"not a principal ID", map[string]any{"principals": []string{"person one"}}, "not a principal ID"},
		{"empty ID", map[string]any{"principals": []string{""}}, "not a principal ID"},
		{"duplicate", map[string]any{"principals": []string{"person-one", "person-one"}}, `principal "person-one" twice`},
		{"empty list", map[string]any{"principals": []string{}}, "omit principals to allow everyone"},
	})
	servers, err := loadMCPServersFrom(t,
		validMCPServer("everyone"),
		withMCP(validMCPServer("some"), map[string]any{"principals": []string{"person-one"}}))
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].Principals != nil || !servers[0].Allows("anyone") {
		t.Fatalf("absent list = %+v", servers[0].Principals)
	}
	if !servers[1].Allows("person-one") || servers[1].Allows("person-two") {
		t.Fatalf("listed = %+v", servers[1].Principals)
	}
}

func TestMCPCatalogRequiresConversation(t *testing.T) {
	servers, err := loadMCPServersFrom(t, validMCPServer("listings"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ListenAddress:   defaultListenAddress,
		ShutdownTimeout: defaultShutdownTimeout,
		DataDir:         t.TempDir(),
		Timezone:        defaultTimezone,
		Telegram:        defaultTelegramConfig(),
		Conversation:    defaultConversationConfig(),
		Observability:   defaultObservabilityConfig(),
		MCP:             MCPConfig{ServersFile: "mcp-servers.json", Servers: servers},
	}
	if err := cfg.Validate(); err == nil || err.Error() != "HOME_AGENT_MCP_SERVERS_FILE: requires HOME_AGENT_CONVERSATION_ENABLED=true" {
		t.Fatalf("catalog without conversation: %v", err)
	}

	cfg.Telegram.Enabled, cfg.Telegram.BotToken = true, "123:token"
	cfg.Telegram.Users = TelegramUsers{{TelegramUserID: 101, PrincipalID: "person-one", DisplayName: "Person One"}}
	cfg.Conversation.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("catalog with conversation: %v", err)
	}
}
