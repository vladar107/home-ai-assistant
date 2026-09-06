package integration

import "regexp"

var integrationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
