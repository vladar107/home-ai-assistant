package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maximumWeatherHomeRunes = 200

type WebConfig struct {
	Enabled bool
	// WeatherHome is the household's place name for forecasts when the user names none; empty
	// means the model asks.
	WeatherHome string
}

func defaultWebConfig() WebConfig {
	return WebConfig{}
}

func loadWebConfig(cfg *WebConfig) error {
	if value := os.Getenv("HOME_AGENT_WEB_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_WEB_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	cfg.WeatherHome = strings.TrimSpace(os.Getenv("HOME_AGENT_WEATHER_HOME"))
	return cfg.validate()
}

func (c WebConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if !utf8.ValidString(c.WeatherHome) || utf8.RuneCountInString(c.WeatherHome) > maximumWeatherHomeRunes ||
		strings.IndexFunc(c.WeatherHome, unicode.IsControl) >= 0 {
		return fmt.Errorf("HOME_AGENT_WEATHER_HOME: must be valid UTF-8, no longer than 200 characters, and free of control characters")
	}
	return nil
}
