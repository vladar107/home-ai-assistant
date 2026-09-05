package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultPiBinary            = "pi"
	defaultPiProvider          = "openai-codex"
	defaultPiVersion           = "0.84.1"
	defaultConversationTimeout = 2 * time.Minute
	minimumConversationTimeout = 5 * time.Second
	maximumConversationTimeout = 10 * time.Minute
	maximumRuntimeValueRunes   = 200
)

type ConversationConfig struct {
	Enabled      bool
	PiBinary     string
	PiNodeBinary string
	PiProvider   string
	PiModel      string
	PiVersion    string
	TurnTimeout  time.Duration
}

func defaultConversationConfig() ConversationConfig {
	return ConversationConfig{
		PiBinary:    defaultPiBinary,
		PiProvider:  defaultPiProvider,
		PiVersion:   defaultPiVersion,
		TurnTimeout: defaultConversationTimeout,
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
	cfg.PiModel = os.Getenv("HOME_AGENT_PI_MODEL")
	cfg.PiVersion = envOrDefault("HOME_AGENT_PI_VERSION", defaultPiVersion)

	if value := os.Getenv("HOME_AGENT_CONVERSATION_TURN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_CONVERSATION_TURN_TIMEOUT: invalid duration")
		}
		cfg.TurnTimeout = timeout
	}

	return cfg.validate()
}

func (c ConversationConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	for name, value := range map[string]string{
		"HOME_AGENT_PI_BINARY":   c.PiBinary,
		"HOME_AGENT_PI_PROVIDER": c.PiProvider,
		"HOME_AGENT_PI_MODEL":    c.PiModel,
		"HOME_AGENT_PI_VERSION":  c.PiVersion,
	} {
		if value != strings.TrimSpace(value) || !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 ||
			utf8.RuneCountInString(value) > maximumRuntimeValueRunes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s: must be trimmed, valid UTF-8, and between 1 and 200 characters", name)
		}
	}
	if c.PiNodeBinary != "" {
		if c.PiNodeBinary != strings.TrimSpace(c.PiNodeBinary) || !utf8.ValidString(c.PiNodeBinary) ||
			utf8.RuneCountInString(c.PiNodeBinary) > maximumRuntimeValueRunes || strings.IndexFunc(c.PiNodeBinary, unicode.IsControl) >= 0 {
			return fmt.Errorf("HOME_AGENT_PI_NODE_BINARY: must be trimmed, valid UTF-8, and no longer than 200 characters")
		}
	}
	if c.TurnTimeout < minimumConversationTimeout || c.TurnTimeout > maximumConversationTimeout {
		return fmt.Errorf("HOME_AGENT_CONVERSATION_TURN_TIMEOUT: must be between 5s and 10m")
	}
	return nil
}
