package integration

import "regexp"

var integrationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ValidName reports whether name can name an integration, such as one a restored switch names.
func ValidName(name string) bool { return integrationNamePattern.MatchString(name) }
