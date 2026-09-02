package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTelegramAPIBaseURL  = "https://api.telegram.org"
	defaultTelegramPollTimeout = 25 * time.Second
	maxTelegramPollTimeout     = 50 * time.Second
	maxTelegramUsers           = 2
)

type TelegramConfig struct {
	Enabled      bool
	APIBaseURL   string
	BotToken     string
	BotTokenFile string
	UsersFile    string
	PollTimeout  time.Duration
	Users        TelegramUsers
}

type TelegramUsers []TelegramUser

type TelegramUser struct {
	TelegramUserID int64  `json:"telegram_user_id"`
	PrincipalID    string `json:"principal_id"`
	DisplayName    string `json:"display_name"`
}

type telegramUsersFile struct {
	Users []TelegramUser `json:"users"`
}

func defaultTelegramConfig() TelegramConfig {
	return TelegramConfig{
		APIBaseURL:  defaultTelegramAPIBaseURL,
		PollTimeout: defaultTelegramPollTimeout,
	}
}

func loadTelegramConfig(cfg *TelegramConfig) error {
	cfg.APIBaseURL = envOrDefault("HOME_AGENT_TELEGRAM_API_BASE_URL", defaultTelegramAPIBaseURL)
	cfg.BotToken = os.Getenv("HOME_AGENT_TELEGRAM_BOT_TOKEN")
	cfg.BotTokenFile = os.Getenv("HOME_AGENT_TELEGRAM_BOT_TOKEN_FILE")
	cfg.UsersFile = os.Getenv("HOME_AGENT_TELEGRAM_USERS_FILE")
	singleUserID := strings.TrimSpace(os.Getenv("HOME_AGENT_TELEGRAM_USER_ID"))

	if value := os.Getenv("HOME_AGENT_TELEGRAM_POLL_TIMEOUT"); value != "" {
		pollTimeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_POLL_TIMEOUT: invalid duration")
		}
		cfg.PollTimeout = pollTimeout
	}

	apiBaseURL, err := normalizeTelegramAPIBaseURL(cfg.APIBaseURL)
	if err != nil {
		return err
	}
	cfg.APIBaseURL = apiBaseURL

	switch {
	case cfg.BotToken != "" && strings.TrimSpace(cfg.BotTokenFile) != "":
		return fmt.Errorf("Telegram token: set HOME_AGENT_TELEGRAM_BOT_TOKEN or HOME_AGENT_TELEGRAM_BOT_TOKEN_FILE, not both")
	case cfg.BotToken == "" && strings.TrimSpace(cfg.BotTokenFile) == "":
		return fmt.Errorf("Telegram token: set HOME_AGENT_TELEGRAM_BOT_TOKEN or HOME_AGENT_TELEGRAM_BOT_TOKEN_FILE when Telegram is enabled")
	}
	switch {
	case strings.TrimSpace(cfg.UsersFile) != "" && singleUserID != "":
		return fmt.Errorf("Telegram users: set HOME_AGENT_TELEGRAM_USER_ID or HOME_AGENT_TELEGRAM_USERS_FILE, not both")
	case strings.TrimSpace(cfg.UsersFile) != "":
		users, err := loadTelegramUsers(cfg.UsersFile)
		if err != nil {
			return err
		}
		cfg.Users = users
	case singleUserID != "":
		telegramUserID, err := strconv.ParseInt(singleUserID, 10, 64)
		if err != nil || telegramUserID <= 0 {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USER_ID: must be a positive integer")
		}
		cfg.Users = TelegramUsers{{
			TelegramUserID: telegramUserID,
			PrincipalID:    "primary",
			DisplayName:    "Primary user",
		}}
	default:
		return fmt.Errorf("Telegram users: set HOME_AGENT_TELEGRAM_USER_ID or HOME_AGENT_TELEGRAM_USERS_FILE when Telegram is enabled")
	}

	return nil
}

func (c TelegramConfig) validate() error {
	if !c.Enabled {
		return nil
	}

	if c.BotToken != "" && strings.TrimSpace(c.BotTokenFile) != "" {
		return fmt.Errorf("Telegram token: set HOME_AGENT_TELEGRAM_BOT_TOKEN or HOME_AGENT_TELEGRAM_BOT_TOKEN_FILE, not both")
	}
	if c.BotToken == "" && strings.TrimSpace(c.BotTokenFile) == "" {
		return fmt.Errorf("Telegram token: set HOME_AGENT_TELEGRAM_BOT_TOKEN or HOME_AGENT_TELEGRAM_BOT_TOKEN_FILE when Telegram is enabled")
	}
	if _, err := normalizeTelegramAPIBaseURL(c.APIBaseURL); err != nil {
		return err
	}
	if c.PollTimeout < time.Second || c.PollTimeout > maxTelegramPollTimeout || c.PollTimeout%time.Second != 0 {
		return fmt.Errorf("HOME_AGENT_TELEGRAM_POLL_TIMEOUT: must be a whole number of seconds between 1s and 50s")
	}
	if err := validateTelegramUsers(c.Users); err != nil {
		return err
	}

	return nil
}

func normalizeTelegramAPIBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return "", fmt.Errorf("HOME_AGENT_TELEGRAM_API_BASE_URL: must be a valid HTTP or HTTPS URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("HOME_AGENT_TELEGRAM_API_BASE_URL: scheme must be HTTPS, or HTTP for localhost or a loopback address")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") {
		return "", fmt.Errorf("HOME_AGENT_TELEGRAM_API_BASE_URL: userinfo, query, and fragment are not allowed")
	}
	if parsed.Scheme == "http" && !isLocalTelegramHost(parsed.Hostname()) {
		return "", fmt.Errorf("HOME_AGENT_TELEGRAM_API_BASE_URL: HTTP is allowed only for localhost or a loopback address")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func isLocalTelegramHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadTelegramUsers(path string) (TelegramUsers, error) {
	file, err := os.Open(path)
	if err != nil {
		return TelegramUsers{}, fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: cannot open configured file")
	}
	defer file.Close()

	var document telegramUsersFile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return TelegramUsers{}, fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return TelegramUsers{}, fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: invalid trailing content")
	}
	if len(document.Users) < 1 || len(document.Users) > maxTelegramUsers {
		return TelegramUsers{}, fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: must contain one or two users")
	}

	users := TelegramUsers(document.Users)
	for i := range users {
		users[i].PrincipalID = strings.TrimSpace(users[i].PrincipalID)
		users[i].DisplayName = strings.TrimSpace(users[i].DisplayName)
	}
	if err := validateTelegramUsers(users); err != nil {
		return TelegramUsers{}, err
	}

	return users, nil
}

func validateTelegramUsers(users TelegramUsers) error {
	if len(users) < 1 || len(users) > maxTelegramUsers {
		return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: must contain one or two users")
	}

	telegramUserIDs := make(map[int64]struct{}, len(users))
	principalIDs := make(map[string]struct{}, len(users))
	for _, user := range users {
		if user.TelegramUserID <= 0 {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: Telegram user IDs must be positive integers")
		}
		if user.PrincipalID != strings.TrimSpace(user.PrincipalID) || utf8.RuneCountInString(user.PrincipalID) < 1 || utf8.RuneCountInString(user.PrincipalID) > 128 {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: principal IDs must be trimmed and between 1 and 128 characters")
		}
		if user.DisplayName != strings.TrimSpace(user.DisplayName) || utf8.RuneCountInString(user.DisplayName) < 1 || utf8.RuneCountInString(user.DisplayName) > 200 {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: display names must be trimmed and between 1 and 200 characters")
		}
		if _, exists := telegramUserIDs[user.TelegramUserID]; exists {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: Telegram user IDs must be distinct")
		}
		telegramUserIDs[user.TelegramUserID] = struct{}{}
		if _, exists := principalIDs[user.PrincipalID]; exists {
			return fmt.Errorf("HOME_AGENT_TELEGRAM_USERS_FILE: principal IDs must be distinct")
		}
		principalIDs[user.PrincipalID] = struct{}{}
	}

	return nil
}
