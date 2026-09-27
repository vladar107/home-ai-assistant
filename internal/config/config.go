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
	defaultTimezone        = "Europe/Amsterdam"
	databaseFilename       = "home-agent.db"
)

type Config struct {
	ListenAddress    string
	ShutdownTimeout  time.Duration
	AllowNonLoopback bool
	DataDir          string
	// Timezone is the IANA zone scheduled jobs use unless a job names its own.
	Timezone      string
	Telegram      TelegramConfig
	Conversation  ConversationConfig
	Notion        NotionConfig
	Gmail         GmailConfig
	NAS           NASConfig
	Web           WebConfig
	Observability ObservabilityConfig
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddress:   envOrDefault("HOME_AGENT_LISTEN_ADDRESS", defaultListenAddress),
		ShutdownTimeout: defaultShutdownTimeout,
		DataDir:         envOrDefault("HOME_AGENT_DATA_DIR", defaultDataDir),
		Timezone:        envOrDefault("HOME_AGENT_TIMEZONE", defaultTimezone),
		Telegram:        defaultTelegramConfig(),
		Conversation:    defaultConversationConfig(),
		Notion:          defaultNotionConfig(),
		Gmail:           defaultGmailConfig(),
		NAS:             defaultNASConfig(),
		Web:             defaultWebConfig(),
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
	if err := loadNASConfig(&cfg.NAS); err != nil {
		return Config{}, err
	}
	if err := loadGmailConfig(&cfg.Gmail); err != nil {
		return Config{}, err
	}
	if err := loadWebConfig(&cfg.Web); err != nil {
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
	if c.Timezone == "" || c.Timezone == "Local" {
		return fmt.Errorf("HOME_AGENT_TIMEZONE: must be an IANA time zone such as Europe/Amsterdam")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("HOME_AGENT_TIMEZONE: unknown time zone %q", c.Timezone)
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
	if err := c.NAS.validate(); err != nil {
		return err
	}
	if c.NAS.Enabled && !c.Conversation.Enabled {
		return fmt.Errorf("HOME_AGENT_NAS_ENABLED: requires HOME_AGENT_CONVERSATION_ENABLED=true")
	}
	if err := c.Gmail.validate(); err != nil {
		return err
	}
	if c.Gmail.Enabled && !c.Conversation.Enabled {
		return fmt.Errorf("HOME_AGENT_GMAIL_ENABLED: requires HOME_AGENT_CONVERSATION_ENABLED=true")
	}
	if err := c.Web.validate(); err != nil {
		return err
	}
	if c.Web.Enabled && !c.Conversation.Enabled {
		return fmt.Errorf("HOME_AGENT_WEB_ENABLED: requires HOME_AGENT_CONVERSATION_ENABLED=true")
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
