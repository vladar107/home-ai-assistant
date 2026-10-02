package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const defaultGoogleRedirectURL = "http://localhost:8080/callback"

type GoogleConfig struct {
	Enabled          bool
	ClientID         string
	ClientSecret     string
	ClientSecretFile string
	// RedirectURL is the loopback address Google sends the browser to after consent; the user
	// pastes it back into the chat, so nothing needs to listen there.
	RedirectURL string
}

func defaultGoogleConfig() GoogleConfig {
	return GoogleConfig{RedirectURL: defaultGoogleRedirectURL}
}

func loadGoogleConfig(cfg *GoogleConfig) error {
	if value := os.Getenv("HOME_AGENT_GOOGLE_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_GOOGLE_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	cfg.ClientID = strings.TrimSpace(os.Getenv("HOME_AGENT_GOOGLE_CLIENT_ID"))
	cfg.ClientSecret = os.Getenv("HOME_AGENT_GOOGLE_CLIENT_SECRET")
	cfg.ClientSecretFile = os.Getenv("HOME_AGENT_GOOGLE_CLIENT_SECRET_FILE")
	cfg.RedirectURL = envOrDefault("HOME_AGENT_GOOGLE_REDIRECT_URL", defaultGoogleRedirectURL)
	return cfg.validate()
}

func (c GoogleConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ClientID == "" || !strings.HasSuffix(c.ClientID, ".apps.googleusercontent.com") {
		return fmt.Errorf("HOME_AGENT_GOOGLE_CLIENT_ID: must be a Google OAuth client ID")
	}
	if (c.ClientSecret == "") == (c.ClientSecretFile == "") {
		return fmt.Errorf("HOME_AGENT_GOOGLE_CLIENT_SECRET or HOME_AGENT_GOOGLE_CLIENT_SECRET_FILE: set exactly one")
	}
	parsed, err := url.Parse(c.RedirectURL)
	if err != nil || parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname()) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("HOME_AGENT_GOOGLE_REDIRECT_URL: must be a plain http loopback address")
	}
	return nil
}
