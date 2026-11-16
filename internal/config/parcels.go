package config

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

var parcelEntryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ParcelsConfig assigns Home Assistant config entries to principals, as the operator lists them in
// HOME_AGENT_PARCEL_SOURCES: groups of "principal=entry_id[,entry_id]" separated by semicolons, such
// as "alice=entry1,entry2;bob=entry3". An entry belongs to at most one principal, and an entry nobody
// is assigned is never read. Empty leaves Parcels on mail alone.
type ParcelsConfig struct {
	Sources map[string][]string
}

func loadParcelsConfig(cfg *ParcelsConfig) error {
	sources, err := parseParcelSources(os.Getenv("HOME_AGENT_PARCEL_SOURCES"))
	if err != nil {
		return err
	}
	cfg.Sources = sources
	return nil
}

func parseParcelSources(value string) (map[string][]string, error) {
	var sources map[string][]string
	for _, group := range strings.Split(value, ";") {
		if group = strings.TrimSpace(group); group == "" {
			continue
		}
		principal, list, found := strings.Cut(group, "=")
		principal = strings.TrimSpace(principal)
		if !found || !grafanaPrincipalPattern.MatchString(principal) {
			return nil, fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: %q is not principal=entry_id[,entry_id]", group)
		}
		if sources == nil {
			sources = map[string][]string{}
		}
		if _, repeated := sources[principal]; repeated {
			return nil, fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: principal %q is listed twice", principal)
		}
		for _, entry := range strings.Split(list, ",") {
			if entry = strings.TrimSpace(entry); entry == "" {
				continue
			}
			if !parcelEntryPattern.MatchString(entry) {
				return nil, fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: %q is not a config entry ID", entry)
			}
			if slices.Contains(sources[principal], entry) {
				return nil, fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: config entry %q is listed twice for %q", entry, principal)
			}
			sources[principal] = append(sources[principal], entry)
		}
		if len(sources[principal]) == 0 {
			return nil, fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: principal %q has no config entry", principal)
		}
	}
	return sources, nil
}

// validate holds the assignments to what startup knows: no entry belongs to two principals, and
// each principal is on the Telegram allowlist.
func (c ParcelsConfig) validate(allowed []string) error {
	owners := map[string]string{}
	for principal, entries := range c.Sources {
		if len(allowed) > 0 && !slices.Contains(allowed, principal) {
			return fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: principal %q is not on the Telegram allowlist", principal)
		}
		for _, entry := range entries {
			if other, taken := owners[entry]; taken && other != principal {
				return fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: config entry %q is assigned to both %q and %q", entry, other, principal)
			}
			owners[entry] = principal
		}
	}
	return nil
}
