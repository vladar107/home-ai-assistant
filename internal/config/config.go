package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	defaultListenAddress   = "127.0.0.1:8080"
	defaultShutdownTimeout = 10 * time.Second
)

type Config struct {
	ListenAddress    string
	ShutdownTimeout  time.Duration
	AllowNonLoopback bool
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddress:   envOrDefault("HOME_AGENT_LISTEN_ADDRESS", defaultListenAddress),
		ShutdownTimeout: defaultShutdownTimeout,
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

	return nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
