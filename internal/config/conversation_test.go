package config

import (
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func loadConversation(t *testing.T, chat, task *string) (ConversationConfig, error) {
	t.Helper()
	t.Setenv("HOME_AGENT_CONVERSATION_ENABLED", "true")
	t.Setenv("HOME_AGENT_PI_MODEL", "")
	t.Setenv("HOME_AGENT_PI_TASK_MODEL", "")
	t.Setenv("HOME_AGENT_PI_HEADSUP_MODEL", "")
	t.Setenv("HOME_AGENT_SCHEDULED_TURN_TIMEOUT", "")
	if chat != nil {
		t.Setenv("HOME_AGENT_PI_MODEL", *chat)
	}
	if task != nil {
		t.Setenv("HOME_AGENT_PI_TASK_MODEL", *task)
	}
	cfg := defaultConversationConfig()
	err := loadConversationConfig(&cfg)
	return cfg, err
}

func ptr(value string) *string { return &value }

func TestModelSettingsAreParsedIntoModelAndEffort(t *testing.T) {
	tests := []struct {
		name     string
		chat     string
		task     *string
		wantChat agentruntime.ModelEffort
		wantTask agentruntime.ModelEffort
	}{
		{"explicit effort", "model-a:high", ptr("model-b:low"), agentruntime.ModelEffort{Model: "model-a", Effort: "high"}, agentruntime.ModelEffort{Model: "model-b", Effort: "low"}},
		{"missing effort is medium", "model-a", ptr("model-b"), agentruntime.ModelEffort{Model: "model-a", Effort: "medium"}, agentruntime.ModelEffort{Model: "model-b", Effort: "medium"}},
		{"model id with a colon", "vendor:model-a:high", ptr("vendor:model-b"), agentruntime.ModelEffort{Model: "vendor:model-a", Effort: "high"}, agentruntime.ModelEffort{Model: "vendor:model-b", Effort: "medium"}},
		{"unknown suffix stays in the id", "model-a:extreme", nil, agentruntime.ModelEffort{Model: "model-a:extreme", Effort: "medium"}, agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "medium"}},
		{"task model defaults", "model-a:low", nil, agentruntime.ModelEffort{Model: "model-a", Effort: "low"}, agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "medium"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConversation(t, &tc.chat, tc.task)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PiModel != tc.wantChat || cfg.PiTaskModel != tc.wantTask {
				t.Errorf("chat = %+v, task = %+v", cfg.PiModel, cfg.PiTaskModel)
			}
		})
	}
}

func TestModelSettingsAreValidatedAtLoad(t *testing.T) {
	tests := []struct {
		name    string
		chat    string
		task    *string
		wantErr string
	}{
		{"empty chat model", "", nil, "HOME_AGENT_PI_MODEL"},
		{"empty chat model id", ":high", nil, "HOME_AGENT_PI_MODEL"},
		{"padded chat model", " model-a", nil, "HOME_AGENT_PI_MODEL"},
		{"empty task model id", "model-a", ptr(":low"), "HOME_AGENT_PI_TASK_MODEL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConversation(t, &tc.chat, tc.task)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want one naming %s", err, tc.wantErr)
			}
		})
	}
}

func TestHeadsUpModelSetting(t *testing.T) {
	cfg, err := loadConversation(t, ptr("model-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "high"}); cfg.PiHeadsUpModel != want {
		t.Errorf("default Heads-up model = %+v, want %+v", cfg.PiHeadsUpModel, want)
	}
	t.Setenv("HOME_AGENT_PI_HEADSUP_MODEL", "model-c:low")
	cfg = defaultConversationConfig()
	if err := loadConversationConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if want := (agentruntime.ModelEffort{Model: "model-c", Effort: "low"}); cfg.PiHeadsUpModel != want {
		t.Errorf("Heads-up model = %+v, want %+v", cfg.PiHeadsUpModel, want)
	}
	t.Setenv("HOME_AGENT_PI_HEADSUP_MODEL", ":high")
	cfg = defaultConversationConfig()
	if err := loadConversationConfig(&cfg); err == nil || !strings.Contains(err.Error(), "HOME_AGENT_PI_HEADSUP_MODEL") {
		t.Errorf("error = %v, want one naming HOME_AGENT_PI_HEADSUP_MODEL", err)
	}
}

func TestScheduledTurnTimeoutSetting(t *testing.T) {
	cfg, err := loadConversation(t, ptr("model-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduledTurnTimeout != 3*time.Minute {
		t.Errorf("default scheduled turn timeout = %s", cfg.ScheduledTurnTimeout)
	}
	tests := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"5m", 5 * time.Minute, false},
		{"10m", 10 * time.Minute, false},
		{"11m", 0, true},
		{"1s", 0, true},
		{"soon", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("HOME_AGENT_SCHEDULED_TURN_TIMEOUT", tc.value)
			cfg := defaultConversationConfig()
			err := loadConversationConfig(&cfg)
			switch {
			case tc.wantErr && (err == nil || !strings.Contains(err.Error(), "HOME_AGENT_SCHEDULED_TURN_TIMEOUT")):
				t.Errorf("error = %v, want one naming HOME_AGENT_SCHEDULED_TURN_TIMEOUT", err)
			case !tc.wantErr && (err != nil || cfg.ScheduledTurnTimeout != tc.want):
				t.Errorf("timeout = %s, error = %v", cfg.ScheduledTurnTimeout, err)
			}
		})
	}
}
