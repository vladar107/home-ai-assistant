package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	flightsAPIKeyFileSizeLimit = 4096
	// defaultFlightsMonthlyUnits leaves 60 of the free plan's 400 units for the operator's own
	// checks and a count lost with the database.
	defaultFlightsMonthlyUnits = 340
	// maximumFlightsMonthlyUnits is far above any plan a household would buy.
	maximumFlightsMonthlyUnits = 1_000_000
)

// FlightsConfig holds the operator's AeroDataBox key on RapidAPI, the units the assistant may spend
// in one quota period, and the day of the month that period starts; the connector is on when a
// key is set.
type FlightsConfig struct {
	APIKey        string
	MonthlyUnits  int
	QuotaResetDay int
}

func (c FlightsConfig) Enabled() bool { return c.APIKey != "" }

func defaultFlightsConfig() FlightsConfig {
	return FlightsConfig{MonthlyUnits: defaultFlightsMonthlyUnits, QuotaResetDay: 1}
}

func loadFlightsConfig(cfg *FlightsConfig) error {
	key := os.Getenv("HOME_AGENT_FLIGHTS_API_KEY")
	file := os.Getenv("HOME_AGENT_FLIGHTS_API_KEY_FILE")
	if key != "" && file != "" {
		return fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY or HOME_AGENT_FLIGHTS_API_KEY_FILE: set at most one")
	}
	if file != "" {
		loaded, err := readFlightsAPIKeyFile(file)
		if err != nil {
			return err
		}
		key = loaded
	}
	cfg.APIKey = strings.TrimSpace(key)
	if value := os.Getenv("HOME_AGENT_FLIGHTS_MONTHLY_UNITS"); value != "" {
		units, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_FLIGHTS_MONTHLY_UNITS: must be a whole number")
		}
		cfg.MonthlyUnits = units
	}
	if value := os.Getenv("HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY"); value != "" {
		day, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY: must be a day of the month")
		}
		cfg.QuotaResetDay = day
	}
	return cfg.validate()
}

func (c FlightsConfig) validate() error {
	if c.APIKey == "" {
		return nil
	}
	if !utf8.ValidString(c.APIKey) || strings.ContainsAny(c.APIKey, " \t\r\n") {
		return fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY or HOME_AGENT_FLIGHTS_API_KEY_FILE: must be a single token without whitespace")
	}
	// One lookup costs two units, so a budget below that could never be spent.
	if c.MonthlyUnits < 2 || c.MonthlyUnits > maximumFlightsMonthlyUnits {
		return fmt.Errorf("HOME_AGENT_FLIGHTS_MONTHLY_UNITS: must be between 2 and %d", maximumFlightsMonthlyUnits)
	}
	if c.QuotaResetDay < 1 || c.QuotaResetDay > 31 {
		return fmt.Errorf("HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY: must be between 1 and 31")
	}
	return nil
}

func readFlightsAPIKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY_FILE: metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Size() > flightsAPIKeyFileSizeLimit {
		return "", fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY_FILE: must be a small regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY_FILE: could not be read")
	}
	key := strings.TrimSpace(string(contents))
	if key == "" {
		return "", fmt.Errorf("HOME_AGENT_FLIGHTS_API_KEY_FILE: does not contain a key")
	}
	return key, nil
}
