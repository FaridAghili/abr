package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var embedPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*\*?$`)
var embedOriginPattern = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+$`)

// Validate accepts literal URL paths and trailing /* prefixes, and origins
// without paths or credentials. This also keeps Caddy tokens/placeholders out.
func (e Embedding) Validate() error {
	if (len(e.Paths) == 0) != (len(e.Origins) == 0) {
		return fmt.Errorf("embedding.paths and embedding.origins must both be set, or both empty")
	}
	seen := map[string]bool{}
	for _, path := range e.Paths {
		if !embedPathPattern.MatchString(path) || (strings.Contains(path, "*") && !strings.HasSuffix(path, "/*")) {
			return fmt.Errorf("invalid embedding path %q: use /banner.html or /ads/*", path)
		}
		for _, part := range strings.Split(path, "/") {
			if part == "." || part == ".." {
				return fmt.Errorf("embedding paths cannot contain dot segments")
			}
		}
		if seen[path] {
			return fmt.Errorf("duplicate embedding path %q", path)
		}
		seen[path] = true
	}
	seen = map[string]bool{}
	for _, origin := range e.Origins {
		if seen[origin] {
			return fmt.Errorf("duplicate embedding origin %q", origin)
		}
		seen[origin] = true
		if origin == "*" {
			if len(e.Origins) != 1 {
				return fmt.Errorf("embedding origin * must be used alone")
			}
			continue
		}
		if origin == "self" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || !embedOriginPattern.MatchString(origin) || u.User != nil || (!validDomain(u.Hostname()) && net.ParseIP(u.Hostname()) == nil) || strings.HasSuffix(origin, ":") {
			return fmt.Errorf("invalid embedding origin %q: use *, self, or an http(s) origin without a path", origin)
		}
		if port := u.Port(); port != "" {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return fmt.Errorf("invalid embedding origin port %q", port)
			}
		}
	}
	return nil
}

func (e Embedding) FrameAncestors() string {
	origins := append([]string(nil), e.Origins...)
	for i, origin := range origins {
		if origin == "self" {
			origins[i] = "'self'"
		}
	}
	return "frame-ancestors " + strings.Join(origins, " ") + ";"
}
