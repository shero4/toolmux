package web

import "regexp"

var (
	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)
