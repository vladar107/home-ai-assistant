package eval

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const minimumScannedLength = 4

// Finding locates a household value in a case or variant file. It never holds the value.
type Finding struct {
	File  string
	Field string
}

// ReadEnvFile returns the key/value pairs of an env file; a missing file is not an error.
func ReadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return values, nil
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// runtimeSetting reports a key that names how the application is deployed or which model runtime
// it uses, not anything about the household. Its values are common words ("home", a model name)
// that a case may well contain.
func runtimeSetting(key string) bool {
	// Scan keys values by their file and name, separated by a NUL.
	if _, name, found := strings.Cut(key, "\x00"); found {
		key = name
	}
	if key == "DEPLOYMENT_ENVIRONMENT" {
		return true
	}
	for _, suffix := range []string{"_PROVIDER", "_MODEL", "_VERSION", "_BINARY"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// HouseholdValues returns the values worth hunting for in a case: at least four characters and
// not purely a number, a boolean or a duration, plus the parts of comma or semicolon lists.
// Runtime settings are left out.
func HouseholdValues(env map[string]string) []string {
	seen := map[string]struct{}{}
	var result []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if len(candidate) < minimumScannedLength || trivial(candidate) {
			return
		}
		lowered := strings.ToLower(candidate)
		if _, duplicate := seen[lowered]; !duplicate {
			seen[lowered] = struct{}{}
			result = append(result, lowered)
		}
	}
	for key, value := range env {
		if runtimeSetting(key) {
			continue
		}
		add(value)
		if strings.ContainsAny(value, ",;") {
			for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' }) {
				add(part)
			}
		}
	}
	sort.Strings(result)
	return result
}

func trivial(value string) bool {
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseBool(value); err == nil {
		return true
	}
	_, err := time.ParseDuration(value)
	return err == nil
}

// ScanFiles matches the household values case-insensitively against every string in the JSON
// files under the given directories; a missing directory is skipped. File paths are reported
// as given.
func ScanFiles(values []string, directories ...string) ([]Finding, error) {
	var findings []Finding
	for _, directory := range directories {
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".json" {
				return nil
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var document any
			if err := json.Unmarshal(contents, &document); err != nil {
				return fmt.Errorf("%s: invalid JSON", path)
			}
			walkStrings(document, "$", func(field, text string) {
				lowered := strings.ToLower(text)
				for _, value := range values {
					if strings.Contains(lowered, value) {
						findings = append(findings, Finding{File: path, Field: field})
						return
					}
				}
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", directory, err)
		}
	}
	return findings, nil
}

func walkStrings(node any, path string, visit func(field, text string)) {
	switch value := node.(type) {
	case string:
		visit(path, value)
	case []any:
		for index, item := range value {
			walkStrings(item, fmt.Sprintf("%s[%d]", path, index), visit)
		}
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkStrings(value[key], path+"."+key, visit)
		}
	}
}

// Scan reads the household values from the env files that exist and scans the case and variant
// directories. found is false when there was no env file to take values from.
func Scan(envPaths []string, directories []string) (findings []Finding, found bool, err error) {
	env := map[string]string{}
	for _, path := range envPaths {
		values, err := ReadEnvFile(path)
		if err != nil {
			return nil, false, err
		}
		if values != nil {
			found = true
		}
		for key, value := range values {
			env[path+"\x00"+key] = value
		}
	}
	if !found {
		return nil, false, nil
	}
	findings, err = ScanFiles(HouseholdValues(env), directories...)
	return findings, true, err
}
