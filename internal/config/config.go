package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddress   = "127.0.0.1:8080"
	defaultShutdownTimeout = 10 * time.Second
	defaultDataDir         = "./data"
	databaseFilename       = "home-agent.db"
)

type Config struct {
	ListenAddress    string
	ShutdownTimeout  time.Duration
	AllowNonLoopback bool
	DataDir          string
	Telegram         TelegramConfig
	Conversation     ConversationConfig
	Notion           NotionConfig
	Observability    ObservabilityConfig
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddress:   envOrDefault("HOME_AGENT_LISTEN_ADDRESS", defaultListenAddress),
		ShutdownTimeout: defaultShutdownTimeout,
		DataDir:         envOrDefault("HOME_AGENT_DATA_DIR", defaultDataDir),
		Telegram:        defaultTelegramConfig(),
		Conversation:    defaultConversationConfig(),
		Notion:          defaultNotionConfig(),
		Observability:   defaultObservabilityConfig(),
	}

	if value := os.Getenv("HOME_AGENT_SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("HOME_AGENT_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = timeout
	}

	if value := os.Getenv("HOME_AGENT_ALLOW_NON_LOOPBACK"); value != "" {
		allow, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("HOME_AGENT_ALLOW_NON_LOOPBACK: %w", err)
		}
		cfg.AllowNonLoopback = allow
	}

	if value := os.Getenv("HOME_AGENT_TELEGRAM_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("HOME_AGENT_TELEGRAM_ENABLED: must be true or false")
		}
		cfg.Telegram.Enabled = enabled
	}

	if err := loadObservabilityConfig(&cfg.Observability); err != nil {
		return Config{}, err
	}

	if cfg.Telegram.Enabled {
		if err := loadTelegramConfig(&cfg.Telegram); err != nil {
			return Config{}, err
		}
	}
	if err := loadConversationConfig(&cfg.Conversation); err != nil {
		return Config{}, err
	}
	if err := loadNotionConfig(&cfg.Notion); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("HOME_AGENT_LISTEN_ADDRESS: %w", err)
	}
	if port == "" {
		return fmt.Errorf("HOME_AGENT_LISTEN_ADDRESS: port is required")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("HOME_AGENT_LISTEN_ADDRESS: port must be between 1 and 65535")
	}

	if !c.AllowNonLoopback {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("HOME_AGENT_LISTEN_ADDRESS: non-loopback address requires HOME_AGENT_ALLOW_NON_LOOPBACK=true")
		}
	}

	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("HOME_AGENT_SHUTDOWN_TIMEOUT: must be greater than zero")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("HOME_AGENT_DATA_DIR: must not be empty")
	}
	if err := c.Telegram.validate(); err != nil {
		return err
	}
	if err := c.Conversation.validate(); err != nil {
		return err
	}
	if c.Conversation.Enabled && !c.Telegram.Enabled {
		return fmt.Errorf("HOME_AGENT_CONVERSATION_ENABLED: requires HOME_AGENT_TELEGRAM_ENABLED=true")
	}
	if err := c.Notion.validate(); err != nil {
		return err
	}
	if c.Notion.Enabled && !c.Conversation.Enabled {
		return fmt.Errorf("HOME_AGENT_NOTION_ENABLED: requires HOME_AGENT_CONVERSATION_ENABLED=true")
	}
	if err := c.Observability.validate(); err != nil {
		return err
	}

	return nil
}

func (c Config) DatabasePath() string {
	return filepath.Join(c.DataDir, databaseFilename)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
