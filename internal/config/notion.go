package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const defaultNotionAPIBaseURL = "https://api.notion.com"

type NotionConfig struct {
	Enabled     bool
	ChatConnect bool
	APIBaseURL  string
}

func defaultNotionConfig() NotionConfig {
	return NotionConfig{APIBaseURL: defaultNotionAPIBaseURL}
}

func loadNotionConfig(cfg *NotionConfig) error {
	if value := os.Getenv("HOME_AGENT_NOTION_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_NOTION_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	if value := os.Getenv("HOME_AGENT_NOTION_CHAT_CONNECT"); value != "" {
		chatConnect, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_NOTION_CHAT_CONNECT: must be true or false")
		}
		cfg.ChatConnect = chatConnect
	}
	cfg.APIBaseURL = envOrDefault("HOME_AGENT_NOTION_API_BASE_URL", defaultNotionAPIBaseURL)
	return cfg.validate()
}

func (c NotionConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(c.APIBaseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("HOME_AGENT_NOTION_API_BASE_URL: must be a plain HTTP or HTTPS URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return fmt.Errorf("HOME_AGENT_NOTION_API_BASE_URL: must use HTTPS, or HTTP for a loopback address")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
