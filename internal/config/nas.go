package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxNASRootsPerPrincipal = 8

// nasRootNamePattern is what the model addresses a root by. It is deliberately narrow: the name
// becomes the first segment of every path the model writes.
var (
	nasRootNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	nasPrincipalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type NASConfig struct {
	Enabled   bool
	RootsFile string
	Roots     NASRoots
}

type NASRoots []NASPrincipalRoots

// NASPrincipalRoots is one principal's view of the NAS: the directories they may reach and which
// writable one a confirmed save lands in when the user names no folder.
type NASPrincipalRoots struct {
	PrincipalID  string    `json:"principal_id"`
	Roots        []NASRoot `json:"roots"`
	DefaultWrite string    `json:"default_write"`
}

// NASRoot is one mounted directory: the name the model addresses it by, the path inside the
// container, and whether a confirmed save may write there. A root is read-only unless the mount
// is read-write too; this flag only stops the connector from trying.
type NASRoot struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable,omitempty"`
}

type nasRootsFile struct {
	Principals []NASPrincipalRoots `json:"principals"`
}

func defaultNASConfig() NASConfig {
	return NASConfig{}
}

func loadNASConfig(cfg *NASConfig) error {
	if value := os.Getenv("HOME_AGENT_NAS_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_NAS_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return nil
	}
	cfg.RootsFile = strings.TrimSpace(os.Getenv("HOME_AGENT_NAS_ROOTS_FILE"))
	if cfg.RootsFile == "" {
		return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: required when the NAS is enabled")
	}
	roots, err := loadNASRoots(cfg.RootsFile)
	if err != nil {
		return err
	}
	cfg.Roots = roots
	return cfg.validate()
}

func loadNASRoots(path string) (NASRoots, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: cannot open configured file")
	}
	defer file.Close()

	var document nasRootsFile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: invalid trailing content")
	}
	if len(document.Principals) == 0 {
		return nil, fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: must configure at least one principal")
	}
	roots := NASRoots(document.Principals)
	for index := range roots {
		roots[index].PrincipalID = strings.TrimSpace(roots[index].PrincipalID)
		roots[index].DefaultWrite = strings.TrimSpace(roots[index].DefaultWrite)
	}
	if err := roots.validate(); err != nil {
		return nil, err
	}
	return roots, nil
}

func (c NASConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	return c.Roots.validate()
}

func (r NASRoots) validate() error {
	seen := make(map[string]struct{}, len(r))
	for _, principal := range r {
		if !nasPrincipalPattern.MatchString(principal.PrincipalID) {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q is not a principal ID", principal.PrincipalID)
		}
		if _, duplicate := seen[principal.PrincipalID]; duplicate {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q appears twice", principal.PrincipalID)
		}
		seen[principal.PrincipalID] = struct{}{}
		if len(principal.Roots) == 0 {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q has no roots", principal.PrincipalID)
		}
		if err := validateNASRootSet(principal.PrincipalID, principal.Roots); err != nil {
			return err
		}
		if err := validateNASDefaultWrite(principal); err != nil {
			return err
		}
	}
	return nil
}

func validateNASRootSet(principalID string, roots []NASRoot) error {
	if len(roots) > maxNASRootsPerPrincipal {
		return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q has more than %d roots", principalID, maxNASRootsPerPrincipal)
	}
	names := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if !nasRootNamePattern.MatchString(root.Name) {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q is not a root name", root.Name)
		}
		if _, duplicate := names[root.Name]; duplicate {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q has two roots named %q", principalID, root.Name)
		}
		names[root.Name] = struct{}{}
		if !filepath.IsAbs(root.Path) || root.Path != filepath.Clean(root.Path) {
			return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q must be a clean absolute path", root.Path)
		}
	}
	return nil
}

func validateNASDefaultWrite(principal NASPrincipalRoots) error {
	if principal.DefaultWrite == "" {
		return nil
	}
	for _, root := range principal.Roots {
		if root.Name == principal.DefaultWrite {
			if !root.Writable {
				return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q names a read-only default_write root", principal.PrincipalID)
			}
			return nil
		}
	}
	return fmt.Errorf("HOME_AGENT_NAS_ROOTS_FILE: %q names a default_write root it does not have", principal.PrincipalID)
}
