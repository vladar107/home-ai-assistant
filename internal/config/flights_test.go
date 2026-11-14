package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlightsConfigIsOffWithoutAKeyAndChecksItsBudget(t *testing.T) {
	cfg := defaultFlightsConfig()
	if err := loadFlightsConfig(&cfg); err != nil || cfg.Enabled() || cfg.MonthlyUnits != 340 || cfg.QuotaResetDay != 1 {
		t.Fatalf("without a key = %+v %v", cfg, err)
	}

	file := filepath.Join(t.TempDir(), "flights-key")
	if err := os.WriteFile(file, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME_AGENT_FLIGHTS_API_KEY_FILE", file)
	t.Setenv("HOME_AGENT_FLIGHTS_MONTHLY_UNITS", "5000")
	t.Setenv("HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY", "17")
	cfg = defaultFlightsConfig()
	if err := loadFlightsConfig(&cfg); err != nil || !cfg.Enabled() || cfg.APIKey != "file-key" || cfg.MonthlyUnits != 5000 || cfg.QuotaResetDay != 17 {
		t.Fatalf("from a file = %+v %v", cfg, err)
	}

	for name, value := range map[string]string{
		"HOME_AGENT_FLIGHTS_MONTHLY_UNITS":   "1",
		"HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY": "32",
		"HOME_AGENT_FLIGHTS_API_KEY":         "also-set",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			cfg := defaultFlightsConfig()
			if err := loadFlightsConfig(&cfg); err == nil || !strings.Contains(err.Error(), "HOME_AGENT_FLIGHTS_") {
				t.Fatalf("%s=%s: %v", name, value, err)
			}
		})
	}
}
