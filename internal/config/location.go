package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maximumLocationContactRunes = 200

// LocationConfig turns on shared locations. The durations bound how long a shared position stays
// usable and how soon it is deleted afterwards; Contact is how Overpass and Transitous can reach
// the operator, sent with their requests once set.
type LocationConfig struct {
	Enabled       bool
	PinTTL        time.Duration
	LiveSilence   time.Duration
	LiveGrace     time.Duration
	PruneInterval time.Duration
	Contact       string
}

type durationSetting struct {
	name     string
	target   *time.Duration
	min, max time.Duration
}

func defaultLocationConfig() LocationConfig {
	return LocationConfig{
		PinTTL:        time.Hour,
		LiveSilence:   time.Hour,
		LiveGrace:     15 * time.Minute,
		PruneInterval: 10 * time.Minute,
	}
}

func (c *LocationConfig) durations() []durationSetting {
	return []durationSetting{
		{"HOME_AGENT_LOCATION_PIN_TTL", &c.PinTTL, 5 * time.Minute, 24 * time.Hour},
		{"HOME_AGENT_LOCATION_LIVE_SILENCE", &c.LiveSilence, 5 * time.Minute, 24 * time.Hour},
		{"HOME_AGENT_LOCATION_LIVE_GRACE", &c.LiveGrace, 0, 2 * time.Hour},
		{"HOME_AGENT_LOCATION_PRUNE_INTERVAL", &c.PruneInterval, time.Minute, time.Hour},
	}
}

func loadLocationConfig(cfg *LocationConfig) error {
	if value := os.Getenv("HOME_AGENT_LOCATION_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_LOCATION_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	for _, setting := range cfg.durations() {
		value := strings.TrimSpace(os.Getenv(setting.name))
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: must be a duration such as 1h or 15m", setting.name)
		}
		*setting.target = duration
	}
	cfg.Contact = strings.TrimSpace(os.Getenv("HOME_AGENT_LOCATION_CONTACT"))
	return cfg.validate()
}

func (c LocationConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	for _, setting := range c.durations() {
		if *setting.target < setting.min || *setting.target > setting.max {
			return fmt.Errorf("%s: must be between %s and %s", setting.name, shortDuration(setting.min), shortDuration(setting.max))
		}
	}
	if !utf8.ValidString(c.Contact) || utf8.RuneCountInString(c.Contact) > maximumLocationContactRunes ||
		strings.IndexFunc(c.Contact, unicode.IsControl) >= 0 || strings.ContainsAny(c.Contact, "()") {
		return fmt.Errorf("HOME_AGENT_LOCATION_CONTACT: must be one line of at most 200 characters without parentheses, such as an email address")
	}
	return nil
}

func shortDuration(duration time.Duration) string {
	if duration == 0 {
		return "0"
	}
	text := duration.String()
	text = strings.TrimSuffix(text, "0s")
	return strings.TrimSuffix(text, "0m")
}
