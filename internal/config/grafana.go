package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

const grafanaTokenFileSizeLimit = 4096

var grafanaPrincipalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// GrafanaConfig holds the operator's read-only Grafana service-account token, Grafana's address,
// and the principals allowed to query it; the feature is on when a token is set. An empty URL
// means the connector's default.
type GrafanaConfig struct {
	Token      string
	URL        string
	Principals []string
}

func (c GrafanaConfig) Enabled() bool { return c.Token != "" }

func defaultGrafanaConfig() GrafanaConfig {
	return GrafanaConfig{}
}

func loadGrafanaConfig(cfg *GrafanaConfig) error {
	token := os.Getenv("HOME_AGENT_GRAFANA_TOKEN")
	file := os.Getenv("HOME_AGENT_GRAFANA_TOKEN_FILE")
	if token != "" && file != "" {
		return fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN or HOME_AGENT_GRAFANA_TOKEN_FILE: set at most one")
	}
	if file != "" {
		loaded, err := readGrafanaTokenFile(file)
		if err != nil {
			return err
		}
		token = loaded
	}
	cfg.Token = strings.TrimSpace(token)
	cfg.URL = strings.TrimSpace(os.Getenv("HOME_AGENT_GRAFANA_URL"))
	cfg.Principals = nil
	for _, principal := range strings.Split(os.Getenv("HOME_AGENT_GRAFANA_PRINCIPALS"), ",") {
		if principal = strings.TrimSpace(principal); principal != "" {
			cfg.Principals = append(cfg.Principals, principal)
		}
	}
	return cfg.validate()
}

func (c GrafanaConfig) validate() error {
	if c.Token != "" && (!utf8.ValidString(c.Token) || strings.ContainsAny(c.Token, " \t\r\n")) {
		return fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN or HOME_AGENT_GRAFANA_TOKEN_FILE: must be a single token without whitespace")
	}
	for _, principal := range c.Principals {
		if !grafanaPrincipalPattern.MatchString(principal) {
			return fmt.Errorf("HOME_AGENT_GRAFANA_PRINCIPALS: %q is not a principal ID", principal)
		}
	}
	// Telemetry includes the agent's own logs, so a token nobody is named for would be either
	// unused or, by a later default, everyone's.
	if c.Token != "" && len(c.Principals) == 0 {
		return fmt.Errorf("HOME_AGENT_GRAFANA_PRINCIPALS: name the principals allowed to query Grafana")
	}
	return nil
}

func readGrafanaTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN_FILE: metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > grafanaTokenFileSizeLimit {
		return "", fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN_FILE: must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN_FILE: could not be read")
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("HOME_AGENT_GRAFANA_TOKEN_FILE: does not contain a token")
	}
	return token, nil
}
