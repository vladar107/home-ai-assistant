package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	defaultPiBinary             = "pi"
	defaultPiProvider           = "openai-codex"
	defaultPiVersion            = "1.0.0"
	defaultPiTaskModel          = "gpt-6.1-sol:medium"
	defaultPiHeadsUpModel       = "gpt-6.1-sol:high"
	defaultPiWebSearchExtension = "/usr/local/lib/home-agent/pi-web-search/src/index.ts"
	defaultConversationTimeout  = 2 * time.Minute
	defaultScheduledTimeout     = 3 * time.Minute
	minimumConversationTimeout  = 5 * time.Second
	maximumConversationTimeout  = 10 * time.Minute
	maximumRuntimeValueRunes    = 200
)

type ConversationConfig struct {
	Enabled      bool
	PiBinary     string
	PiNodeBinary string
	PiProvider   string
	// PiModel is the chat default, PiTaskModel the background task default, and PiHeadsUpModel
	// the Heads-up job's default, each parsed from "<model>[:<effort>]".
	PiModel        agentruntime.ModelEffort
	PiTaskModel    agentruntime.ModelEffort
	PiHeadsUpModel agentruntime.ModelEffort
	PiVersion      string
	// PiWebSearchExtension is the vendored search extension's entry file, loaded only for
	// principals with web access on.
	PiWebSearchExtension string
	TurnTimeout          time.Duration
	// ScheduledTurnTimeout bounds a scheduled model turn instead of TurnTimeout.
	ScheduledTurnTimeout time.Duration
}

func defaultConversationConfig() ConversationConfig {
	return ConversationConfig{
		PiBinary:             defaultPiBinary,
		PiProvider:           defaultPiProvider,
		PiVersion:            defaultPiVersion,
		PiWebSearchExtension: defaultPiWebSearchExtension,
		TurnTimeout:          defaultConversationTimeout,
		ScheduledTurnTimeout: defaultScheduledTimeout,
	}
}

func loadConversationConfig(cfg *ConversationConfig) error {
	if value := os.Getenv("HOME_AGENT_CONVERSATION_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_CONVERSATION_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}

	cfg.PiBinary = envOrDefault("HOME_AGENT_PI_BINARY", defaultPiBinary)
	cfg.PiNodeBinary = os.Getenv("HOME_AGENT_PI_NODE_BINARY")
	cfg.PiProvider = envOrDefault("HOME_AGENT_PI_PROVIDER", defaultPiProvider)
	var err error
	if cfg.PiModel, err = parseModelSetting("HOME_AGENT_PI_MODEL", os.Getenv("HOME_AGENT_PI_MODEL")); err != nil {
		return err
	}
	if cfg.PiTaskModel, err = parseModelSetting("HOME_AGENT_PI_TASK_MODEL", envOrDefault("HOME_AGENT_PI_TASK_MODEL", defaultPiTaskModel)); err != nil {
		return err
	}
	if cfg.PiHeadsUpModel, err = parseModelSetting("HOME_AGENT_PI_HEADSUP_MODEL", envOrDefault("HOME_AGENT_PI_HEADSUP_MODEL", defaultPiHeadsUpModel)); err != nil {
		return err
	}
	cfg.PiVersion = envOrDefault("HOME_AGENT_PI_VERSION", defaultPiVersion)
	cfg.PiWebSearchExtension = envOrDefault("HOME_AGENT_PI_WEB_SEARCH_EXTENSION", defaultPiWebSearchExtension)

	if value := os.Getenv("HOME_AGENT_CONVERSATION_TURN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_CONVERSATION_TURN_TIMEOUT: invalid duration")
		}
		cfg.TurnTimeout = timeout
	}
	if value := os.Getenv("HOME_AGENT_SCHEDULED_TURN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_SCHEDULED_TURN_TIMEOUT: invalid duration")
		}
		cfg.ScheduledTurnTimeout = timeout
	}

	return cfg.validate()
}

func parseModelSetting(name, value string) (agentruntime.ModelEffort, error) {
	if err := validateRuntimeValue(name, value); err != nil {
		return agentruntime.ModelEffort{}, err
	}
	parsed, err := agentruntime.ParseModelEffort(value)
	if err != nil {
		return agentruntime.ModelEffort{}, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func validateRuntimeValue(name, value string) error {
	if value != strings.TrimSpace(value) || !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 ||
		utf8.RuneCountInString(value) > maximumRuntimeValueRunes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s: must be trimmed, valid UTF-8, and between 1 and 200 characters", name)
	}
	return nil
}

func (c ConversationConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	for name, value := range map[string]string{
		"HOME_AGENT_PI_BINARY":   c.PiBinary,
		"HOME_AGENT_PI_PROVIDER": c.PiProvider,
		"HOME_AGENT_PI_VERSION":  c.PiVersion,
	} {
		if err := validateRuntimeValue(name, value); err != nil {
			return err
		}
	}
	if c.PiNodeBinary != "" {
		if c.PiNodeBinary != strings.TrimSpace(c.PiNodeBinary) || !utf8.ValidString(c.PiNodeBinary) ||
			utf8.RuneCountInString(c.PiNodeBinary) > maximumRuntimeValueRunes || strings.IndexFunc(c.PiNodeBinary, unicode.IsControl) >= 0 {
			return fmt.Errorf("HOME_AGENT_PI_NODE_BINARY: must be trimmed, valid UTF-8, and no longer than 200 characters")
		}
	}
	if !filepath.IsAbs(c.PiWebSearchExtension) || c.PiWebSearchExtension != filepath.Clean(c.PiWebSearchExtension) ||
		!utf8.ValidString(c.PiWebSearchExtension) || strings.IndexFunc(c.PiWebSearchExtension, unicode.IsControl) >= 0 {
		return fmt.Errorf("HOME_AGENT_PI_WEB_SEARCH_EXTENSION: must be a clean absolute path")
	}
	if c.TurnTimeout < minimumConversationTimeout || c.TurnTimeout > maximumConversationTimeout {
		return fmt.Errorf("HOME_AGENT_CONVERSATION_TURN_TIMEOUT: must be between 5s and 10m")
	}
	if c.ScheduledTurnTimeout < minimumConversationTimeout || c.ScheduledTurnTimeout > maximumConversationTimeout {
		return fmt.Errorf("HOME_AGENT_SCHEDULED_TURN_TIMEOUT: must be between 5s and 10m")
	}
	return nil
}
