package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setAssistantMailEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	values := map[string]string{
		"HOME_AGENT_ASSISTANT_MAIL_ENABLED":       "true",
		"HOME_AGENT_ASSISTANT_MAIL_ADDRESS":       "assistant@example.com",
		"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME":  "Helper (AI assistant)",
		"HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID":     "test.apps.googleusercontent.com",
		"HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET": "client-secret",
		"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN": "refresh-token",
		"HOME_AGENT_ASSISTANT_MAIL_TAGS":          "alice=aa1,aa0;bob=bb1",
	}
	for name, value := range overrides {
		values[name] = value
	}
	for _, name := range []string{
		"HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET_FILE",
		"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE",
		"HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT",
		"HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL",
	} {
		t.Setenv(name, "")
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
}

func TestAssistantMailLoadsAFullConfig(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setAssistantMailEnv(t, map[string]string{
		"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN":      "",
		"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE": secretFile,
		"HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT":   "25",
		"HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL":      "10m",
	})
	cfg := defaultAssistantMailConfig()
	if err := loadAssistantMailConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	want := AssistantMailConfig{
		Enabled:        true,
		Address:        "assistant@example.com",
		DisplayName:    "Helper (AI assistant)",
		ClientID:       "test.apps.googleusercontent.com",
		ClientSecret:   "client-secret",
		RefreshToken:   "file-token",
		Tags:           map[string][]string{"alice": {"aa1", "aa0"}, "bob": {"bb1"}},
		DailySendLimit: 25,
		PollInterval:   10 * time.Minute,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestAssistantMailTagLookups(t *testing.T) {
	cfg := AssistantMailConfig{Tags: map[string][]string{"alice": {"aa1", "aa0"}, "bob": {"bb1"}}}
	if principal, ok := cfg.PrincipalForTag("aa0"); !ok || principal != "alice" {
		t.Fatalf("retired tag routes to %q, %v", principal, ok)
	}
	if principal, ok := cfg.PrincipalForTag("bb1"); !ok || principal != "bob" {
		t.Fatalf("tag routes to %q, %v", principal, ok)
	}
	if _, ok := cfg.PrincipalForTag("zz9"); ok {
		t.Fatal("unknown tag routed")
	}
	if tag, ok := cfg.CurrentTag("alice"); !ok || tag != "aa1" {
		t.Fatalf("current tag = %q, %v", tag, ok)
	}
	if _, ok := cfg.CurrentTag("carol"); ok {
		t.Fatal("untagged principal has a tag")
	}
}

func TestAssistantMailDefaultsAndDisabled(t *testing.T) {
	setAssistantMailEnv(t, nil)
	cfg := defaultAssistantMailConfig()
	if err := loadAssistantMailConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DailySendLimit != 10 || cfg.PollInterval != 5*time.Minute {
		t.Fatalf("defaults = %d %s", cfg.DailySendLimit, cfg.PollInterval)
	}

	setAssistantMailEnv(t, map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ENABLED": "false", "HOME_AGENT_ASSISTANT_MAIL_ADDRESS": ""})
	cfg = defaultAssistantMailConfig()
	if err := loadAssistantMailConfig(&cfg); err != nil || cfg.Enabled {
		t.Fatalf("disabled = %+v %v", cfg, err)
	}
	setAssistantMailEnv(t, map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ENABLED": "maybe"})
	if err := loadAssistantMailConfig(&cfg); err == nil || !strings.Contains(err.Error(), "HOME_AGENT_ASSISTANT_MAIL_ENABLED") {
		t.Fatalf("bad flag: %v", err)
	}
}

func TestAssistantMailRejectsInvalidSettings(t *testing.T) {
	bothFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(bothFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"tag too short", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=a"}, "HOME_AGENT_ASSISTANT_MAIL_TAGS"},
		{"tag uppercase", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=Aa1"}, "HOME_AGENT_ASSISTANT_MAIL_TAGS"},
		{"tag with dash", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=aa-1"}, "HOME_AGENT_ASSISTANT_MAIL_TAGS"},
		{"tag too long", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=" + strings.Repeat("a", 33)}, "HOME_AGENT_ASSISTANT_MAIL_TAGS"},
		{"tag across principals", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=aa1;bob=bb1,aa1"}, "assigned to both"},
		{"tag repeated for one principal", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=aa1,aa1"}, "assigned to both"},
		{"principal repeated", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice=aa1;alice=aa2"}, "listed twice"},
		{"principal without tags", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "alice="}, "has no tag"},
		{"malformed group", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": "aa1"}, "principal=tag"},
		{"no tags at all", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_TAGS": ""}, "at least one principal"},
		{"missing client secret", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET": ""}, "HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET"},
		{"missing refresh token", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN": ""}, "HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN"},
		{"client secret value and file", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET_FILE": bothFile}, "set exactly one"},
		{"refresh token value and file", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE": bothFile}, "set exactly one"},
		{"client secret with whitespace", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET": "client secret"}, "without whitespace"},
		{"refresh token with newline", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN": "refresh\ntoken"}, "without whitespace"},
		{"refresh token file absent", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN": "", "HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE": filepath.Join(t.TempDir(), "absent")}, "HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE"},
		{"display name with LF", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": "Helper\nBcc: x"}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"display name with CR", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": "Helper\rx"}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"display name with tab", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": "Helper\tx"}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"display name invalid UTF-8", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": "Helper\xff"}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"display name empty", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": ""}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"display name too long", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME": strings.Repeat("é", 65)}, "HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"},
		{"address with plus", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ADDRESS": "assistant+aa1@example.com"}, "plus tag"},
		{"address with display name", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ADDRESS": "Helper <assistant@example.com>"}, "HOME_AGENT_ASSISTANT_MAIL_ADDRESS"},
		{"address uppercase", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ADDRESS": "Assistant@example.com"}, "HOME_AGENT_ASSISTANT_MAIL_ADDRESS"},
		{"address missing", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_ADDRESS": ""}, "HOME_AGENT_ASSISTANT_MAIL_ADDRESS"},
		{"client id not Google", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID": "test.example.com"}, "HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID"},
		{"limit zero", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT": "0"}, "HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT"},
		{"limit too high", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT": "101"}, "HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT"},
		{"limit not a number", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT": "many"}, "HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT"},
		{"interval too short", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL": "30s"}, "HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL"},
		{"interval too long", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL": "2h"}, "HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL"},
		{"interval not a duration", map[string]string{"HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL": "soon"}, "HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAssistantMailEnv(t, test.overrides)
			cfg := defaultAssistantMailConfig()
			err := loadAssistantMailConfig(&cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			for _, secret := range []string{"client-secret", "refresh-token", "client secret", "refresh\ntoken"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error echoes a secret: %v", err)
				}
			}
		})
	}
}

func TestAssistantMailRequiresConversationAndAllowlistedPrincipals(t *testing.T) {
	cfg := Config{
		ListenAddress:   defaultListenAddress,
		ShutdownTimeout: defaultShutdownTimeout,
		DataDir:         t.TempDir(),
		Timezone:        defaultTimezone,
		Telegram:        defaultTelegramConfig(),
		Conversation:    defaultConversationConfig(),
		Observability:   defaultObservabilityConfig(),
		AssistantMail: AssistantMailConfig{
			Enabled:        true,
			Address:        "assistant@example.com",
			DisplayName:    "Helper (AI assistant)",
			ClientID:       "test.apps.googleusercontent.com",
			ClientSecret:   "client-secret",
			RefreshToken:   "refresh-token",
			Tags:           map[string][]string{"alice": {"aa1"}},
			DailySendLimit: 10,
			PollInterval:   5 * time.Minute,
		},
	}
	if err := cfg.Validate(); err == nil || err.Error() != "HOME_AGENT_ASSISTANT_MAIL_ENABLED: requires HOME_AGENT_CONVERSATION_ENABLED=true" {
		t.Fatalf("mailbox without conversation: %v", err)
	}

	cfg.Telegram.Enabled, cfg.Telegram.BotToken = true, "123:token"
	cfg.Telegram.Users = TelegramUsers{{TelegramUserID: 101, PrincipalID: "bob", DisplayName: "Bob"}}
	cfg.Conversation.Enabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not on the Telegram allowlist") {
		t.Fatalf("tag for an unknown principal: %v", err)
	}

	cfg.Telegram.Users = TelegramUsers{{TelegramUserID: 101, PrincipalID: "alice", DisplayName: "Alice"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid mailbox: %v", err)
	}
}
