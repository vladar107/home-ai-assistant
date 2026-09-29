package config

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const nsAPIKeyFileSizeLimit = 4096

// NSConfig holds the operator's NS API Portal subscription key; the feature is on when one is set.
type NSConfig struct {
	APIKey string
}

func (c NSConfig) Enabled() bool { return c.APIKey != "" }

func defaultNSConfig() NSConfig {
	return NSConfig{}
}

func loadNSConfig(cfg *NSConfig) error {
	key := os.Getenv("HOME_AGENT_NS_API_KEY")
	file := os.Getenv("HOME_AGENT_NS_API_KEY_FILE")
	if key != "" && file != "" {
		return fmt.Errorf("HOME_AGENT_NS_API_KEY or HOME_AGENT_NS_API_KEY_FILE: set at most one")
	}
	if file != "" {
		loaded, err := readNSAPIKeyFile(file)
		if err != nil {
			return err
		}
		key = loaded
	}
	cfg.APIKey = strings.TrimSpace(key)
	return cfg.validate()
}

func (c NSConfig) validate() error {
	if c.APIKey != "" && (!utf8.ValidString(c.APIKey) || strings.ContainsAny(c.APIKey, " \t\r\n")) {
		return fmt.Errorf("HOME_AGENT_NS_API_KEY or HOME_AGENT_NS_API_KEY_FILE: must be a single token without whitespace")
	}
	return nil
}

func readNSAPIKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_NS_API_KEY_FILE: metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > nsAPIKeyFileSizeLimit {
		return "", fmt.Errorf("HOME_AGENT_NS_API_KEY_FILE: must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_NS_API_KEY_FILE: could not be read")
	}
	key := strings.TrimSpace(string(contents))
	if key == "" {
		return "", fmt.Errorf("HOME_AGENT_NS_API_KEY_FILE: does not contain a key")
	}
	return key, nil
}
