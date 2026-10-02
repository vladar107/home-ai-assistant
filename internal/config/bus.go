package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maximumBusContactRunes = 200

// BusConfig turns on bus information. HomeStop is a timing point code of the household's home stop;
// Contact is how Transitous can reach the operator, sent with its requests once set.
type BusConfig struct {
	Enabled  bool
	HomeStop string
	Contact  string
}

func defaultBusConfig() BusConfig {
	return BusConfig{}
}

func loadBusConfig(cfg *BusConfig) error {
	if value := os.Getenv("HOME_AGENT_BUS_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_BUS_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	cfg.HomeStop = strings.TrimSpace(os.Getenv("HOME_AGENT_BUS_HOME_STOP"))
	cfg.Contact = strings.TrimSpace(os.Getenv("HOME_AGENT_BUS_CONTACT"))
	return cfg.validate()
}

func (c BusConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.IndexFunc(c.HomeStop, func(r rune) bool { return r < '0' || r > '9' }) >= 0 || len(c.HomeStop) > 20 {
		return fmt.Errorf("HOME_AGENT_BUS_HOME_STOP: must be a timing point code made of digits")
	}
	if !utf8.ValidString(c.Contact) || utf8.RuneCountInString(c.Contact) > maximumBusContactRunes ||
		strings.IndexFunc(c.Contact, unicode.IsControl) >= 0 || strings.ContainsAny(c.Contact, "()") {
		return fmt.Errorf("HOME_AGENT_BUS_CONTACT: must be one line of at most 200 characters without parentheses, such as an email address")
	}
	return nil
}
