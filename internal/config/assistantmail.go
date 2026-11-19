package config

import (
	"fmt"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	assistantMailSecretFileSizeLimit = 4096
	assistantMailDisplayNameMaxRunes = 64
	defaultAssistantMailSendLimit    = 10
	defaultAssistantMailPollInterval = 5 * time.Minute
	minAssistantMailPollInterval     = time.Minute
	maxAssistantMailPollInterval     = time.Hour
	maxAssistantMailSendLimit        = 100
)

var assistantMailTagPattern = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

// AssistantMailConfig describes the household's one assistant mailbox. Tags assign each principal
// plus tags on its address, as the operator lists them in HOME_AGENT_ASSISTANT_MAIL_TAGS: groups of
// "principal=tag[,tag]" separated by semicolons. The first tag is the principal's current address;
// later ones are retired tags that stay routed.
type AssistantMailConfig struct {
	Enabled      bool
	Address      string
	DisplayName  string
	ClientID     string
	ClientSecret string
	RefreshToken string
	Tags         map[string][]string
	// DailySendLimit is the most messages one principal may start sending per day.
	DailySendLimit int
	PollInterval   time.Duration
}

func defaultAssistantMailConfig() AssistantMailConfig {
	return AssistantMailConfig{
		DailySendLimit: defaultAssistantMailSendLimit,
		PollInterval:   defaultAssistantMailPollInterval,
	}
}

// PrincipalForTag returns the principal a plus tag belongs to, retired tags included.
func (c AssistantMailConfig) PrincipalForTag(tag string) (string, bool) {
	for principal, tags := range c.Tags {
		if slices.Contains(tags, tag) {
			return principal, true
		}
	}
	return "", false
}

// CurrentTag returns the tag a principal's address currently carries.
func (c AssistantMailConfig) CurrentTag(principal string) (string, bool) {
	tags := c.Tags[principal]
	if len(tags) == 0 {
		return "", false
	}
	return tags[0], true
}

func loadAssistantMailConfig(cfg *AssistantMailConfig) error {
	if value := os.Getenv("HOME_AGENT_ASSISTANT_MAIL_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	cfg.Address = strings.TrimSpace(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_ADDRESS"))
	cfg.DisplayName = strings.TrimSpace(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME"))
	cfg.ClientID = strings.TrimSpace(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID"))
	var err error
	if cfg.ClientSecret, err = loadAssistantMailSecret("HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET"); err != nil {
		return err
	}
	if cfg.RefreshToken, err = loadAssistantMailSecret("HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN"); err != nil {
		return err
	}
	if cfg.Tags, err = parseAssistantMailTags(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_TAGS")); err != nil {
		return err
	}
	if value := strings.TrimSpace(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT: must be a whole number")
		}
		cfg.DailySendLimit = limit
	}
	if value := strings.TrimSpace(os.Getenv("HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL")); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL: must be a duration such as 5m")
		}
		cfg.PollInterval = interval
	}
	return cfg.validate()
}

// loadAssistantMailSecret reads a secret from NAME or from the file NAME_FILE, never both.
func loadAssistantMailSecret(name string) (string, error) {
	value := os.Getenv(name)
	file := os.Getenv(name + "_FILE")
	if (value == "") == (file == "") {
		return "", fmt.Errorf("%s or %s_FILE: set exactly one", name, name)
	}
	if file != "" {
		return readAssistantMailSecretFile(name+"_FILE", file)
	}
	return checkAssistantMailSecret(name, strings.TrimSpace(value))
}

func checkAssistantMailSecret(name, secret string) (string, error) {
	if !utf8.ValidString(secret) || strings.ContainsAny(secret, " \t\r\n") {
		return "", fmt.Errorf("%s: must be a single value without whitespace", name)
	}
	return secret, nil
}

func readAssistantMailSecretFile(name, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: metadata could not be read", name)
	}
	if !info.Mode().IsRegular() || info.Size() > assistantMailSecretFileSizeLimit {
		return "", fmt.Errorf("%s: must be a small regular file", name)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: could not be read", name)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s: does not contain a value", name)
	}
	return checkAssistantMailSecret(name, secret)
}

func parseAssistantMailTags(value string) (map[string][]string, error) {
	var tags map[string][]string
	owners := map[string]string{}
	for _, group := range strings.Split(value, ";") {
		if group = strings.TrimSpace(group); group == "" {
			continue
		}
		principal, list, found := strings.Cut(group, "=")
		principal = strings.TrimSpace(principal)
		if !found || !grafanaPrincipalPattern.MatchString(principal) {
			return nil, fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: %q is not principal=tag[,tag]", group)
		}
		if tags == nil {
			tags = map[string][]string{}
		}
		if _, repeated := tags[principal]; repeated {
			return nil, fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: principal %q is listed twice", principal)
		}
		for _, tag := range strings.Split(list, ",") {
			if tag = strings.TrimSpace(tag); tag == "" {
				continue
			}
			if !assistantMailTagPattern.MatchString(tag) {
				return nil, fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: %q is not 2 to 32 lowercase letters and digits", tag)
			}
			if other, taken := owners[tag]; taken {
				return nil, fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: tag %q is assigned to both %q and %q", tag, other, principal)
			}
			owners[tag] = principal
			tags[principal] = append(tags[principal], tag)
		}
		if len(tags[principal]) == 0 {
			return nil, fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: principal %q has no tag", principal)
		}
	}
	return tags, nil
}

func (c AssistantMailConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if err := validateAssistantMailAddress(c.Address); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(c.DisplayName); !utf8.ValidString(c.DisplayName) || n < 1 || n > assistantMailDisplayNameMaxRunes || strings.ContainsFunc(c.DisplayName, unicode.IsControl) {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_DISPLAY_NAME: must be 1 to %d characters without control characters", assistantMailDisplayNameMaxRunes)
	}
	if !strings.HasSuffix(c.ClientID, ".apps.googleusercontent.com") {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_CLIENT_ID: must be a Google OAuth client ID")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET or HOME_AGENT_ASSISTANT_MAIL_CLIENT_SECRET_FILE: set exactly one")
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN or HOME_AGENT_ASSISTANT_MAIL_REFRESH_TOKEN_FILE: set exactly one")
	}
	if len(c.Tags) == 0 {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: list at least one principal")
	}
	if c.DailySendLimit < 1 || c.DailySendLimit > maxAssistantMailSendLimit {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT: must be between 1 and %d", maxAssistantMailSendLimit)
	}
	if c.PollInterval < minAssistantMailPollInterval || c.PollInterval > maxAssistantMailPollInterval {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_POLL_INTERVAL: must be between %s and %s", minAssistantMailPollInterval, maxAssistantMailPollInterval)
	}
	return nil
}

func validateAssistantMailAddress(address string) error {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Name != "" || parsed.Address != address || address != strings.ToLower(address) {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_ADDRESS: must be one plain lowercase email address")
	}
	if local, _, _ := strings.Cut(address, "@"); strings.Contains(local, "+") {
		return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_ADDRESS: must not contain a plus tag")
	}
	return nil
}

// validateAllowlist holds each tagged principal to the Telegram allowlist.
func (c AssistantMailConfig) validateAllowlist(allowed []string) error {
	if !c.Enabled {
		return nil
	}
	for principal := range c.Tags {
		if !slices.Contains(allowed, principal) {
			return fmt.Errorf("HOME_AGENT_ASSISTANT_MAIL_TAGS: principal %q is not on the Telegram allowlist", principal)
		}
	}
	return nil
}
