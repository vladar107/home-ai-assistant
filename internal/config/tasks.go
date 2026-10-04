package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultTaskTimeout = 60 * time.Minute
	minimumTaskTimeout = time.Minute
	maximumTaskTimeout = 6 * time.Hour
)

// TasksConfig turns on background tasks, which run long requests beside the chat. Timeout caps one
// task's wall time.
type TasksConfig struct {
	Enabled bool
	Timeout time.Duration
}

func defaultTasksConfig() TasksConfig {
	return TasksConfig{Timeout: defaultTaskTimeout}
}

func loadTasksConfig(cfg *TasksConfig) error {
	if value := os.Getenv("HOME_AGENT_TASKS_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_TASKS_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	if value := os.Getenv("HOME_AGENT_TASK_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_TASK_TIMEOUT: invalid duration")
		}
		cfg.Timeout = timeout
	}
	return cfg.validate()
}

func (c TasksConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Timeout < minimumTaskTimeout || c.Timeout > maximumTaskTimeout {
		return fmt.Errorf("HOME_AGENT_TASK_TIMEOUT: must be between 1m and 6h")
	}
	return nil
}
