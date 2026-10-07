package config

import (
	"fmt"
	"os"
	"strconv"
)

// WatchesConfig turns on watches, which check a departure, route, or stations during a daily
// window and message the user only when something changes.
type WatchesConfig struct {
	Enabled bool
}

func loadWatchesConfig(cfg *WatchesConfig) error {
	if value := os.Getenv("HOME_AGENT_WATCHES_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_WATCHES_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	return nil
}
