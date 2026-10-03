package config

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const haTokenFileSizeLimit = 4096

// HAConfig holds the household's Home Assistant access token and address; the feature is on when
// a token is set. An empty URL means the connector's default.
type HAConfig struct {
	Token string
	URL   string
}

func (c HAConfig) Enabled() bool { return c.Token != "" }

func defaultHAConfig() HAConfig {
	return HAConfig{}
}

func loadHAConfig(cfg *HAConfig) error {
	token := os.Getenv("HOME_AGENT_HA_TOKEN")
	file := os.Getenv("HOME_AGENT_HA_TOKEN_FILE")
	if token != "" && file != "" {
		return fmt.Errorf("HOME_AGENT_HA_TOKEN or HOME_AGENT_HA_TOKEN_FILE: set at most one")
	}
	if file != "" {
		loaded, err := readHATokenFile(file)
		if err != nil {
			return err
		}
		token = loaded
	}
	cfg.Token = strings.TrimSpace(token)
	cfg.URL = strings.TrimSpace(os.Getenv("HOME_AGENT_HA_URL"))
	return cfg.validate()
}

func (c HAConfig) validate() error {
	if c.Token != "" && (!utf8.ValidString(c.Token) || strings.ContainsAny(c.Token, " \t\r\n")) {
		return fmt.Errorf("HOME_AGENT_HA_TOKEN or HOME_AGENT_HA_TOKEN_FILE: must be a single token without whitespace")
	}
	return nil
}

func readHATokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_HA_TOKEN_FILE: metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > haTokenFileSizeLimit {
		return "", fmt.Errorf("HOME_AGENT_HA_TOKEN_FILE: must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_HA_TOKEN_FILE: could not be read")
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("HOME_AGENT_HA_TOKEN_FILE: does not contain a token")
	}
	return token, nil
}
