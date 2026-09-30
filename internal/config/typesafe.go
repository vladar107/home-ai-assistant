package config

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const typeSafeKeyFileSizeLimit = 4096

// TypeSafeConfig holds the operator's TypeSafe API key; judgments are on when one is set.
type TypeSafeConfig struct {
	APIKey string
}

func (c TypeSafeConfig) Enabled() bool { return c.APIKey != "" }

func defaultTypeSafeConfig() TypeSafeConfig {
	return TypeSafeConfig{}
}

func loadTypeSafeConfig(cfg *TypeSafeConfig) error {
	key := os.Getenv("HOME_AGENT_TYPESAFEAI_KEY")
	file := os.Getenv("HOME_AGENT_TYPESAFEAI_KEY_FILE")
	if key != "" && file != "" {
		return fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY or HOME_AGENT_TYPESAFEAI_KEY_FILE: set at most one")
	}
	if file != "" {
		loaded, err := readTypeSafeKeyFile(file)
		if err != nil {
			return err
		}
		key = loaded
	}
	cfg.APIKey = strings.TrimSpace(key)
	return cfg.validate()
}

func (c TypeSafeConfig) validate() error {
	if c.APIKey != "" && (!utf8.ValidString(c.APIKey) || strings.ContainsAny(c.APIKey, " \t\r\n")) {
		return fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY or HOME_AGENT_TYPESAFEAI_KEY_FILE: must be a single token without whitespace")
	}
	return nil
}

func readTypeSafeKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY_FILE: metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > typeSafeKeyFileSizeLimit {
		return "", fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY_FILE: must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY_FILE: could not be read")
	}
	key := strings.TrimSpace(string(contents))
	if key == "" {
		return "", fmt.Errorf("HOME_AGENT_TYPESAFEAI_KEY_FILE: does not contain a key")
	}
	return key, nil
}
