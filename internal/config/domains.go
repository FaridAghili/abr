package config

import (
	"fmt"
	"strings"
)

const (
	CanonicalAsEntered = "as-entered"
	CanonicalWWW       = "www"
	CanonicalNonWWW    = "non-www"
)

// WithCanonicalHost resolves a registration preference into the existing domain
// and aliases fields. Only the entered hostname and its www counterpart change;
// no preference is inherited by other applications or additional serving domains.
func (a App) WithCanonicalHost(preference string) (App, error) {
	switch preference {
	case "", CanonicalAsEntered:
		return a, nil
	case CanonicalWWW, CanonicalNonWWW:
	default:
		return a, fmt.Errorf("canonical host must be as-entered, www or non-www")
	}
	if !validDomain(a.Domain) {
		return a, fmt.Errorf("invalid main domain %q", a.Domain)
	}
	base := strings.TrimPrefix(strings.ToLower(a.Domain), "www.")
	www := "www." + base
	if !validDomain(base) || !validDomain(www) {
		return a, fmt.Errorf("invalid www/non-www domain pair for %q", a.Domain)
	}
	canonical, redirect := www, base
	if preference == CanonicalNonWWW {
		canonical, redirect = base, www
	}
	for _, domain := range a.Domains {
		if strings.EqualFold(domain, canonical) || strings.EqualFold(domain, redirect) {
			return a, fmt.Errorf("canonical host conflicts with additional serving domain %q; remove it from --serving-domain", domain)
		}
	}
	aliases := make([]string, 0, len(a.Aliases)+1)
	for _, domain := range a.Aliases {
		if !strings.EqualFold(domain, canonical) && !strings.EqualFold(domain, redirect) {
			aliases = append(aliases, domain)
		}
	}
	a.Domain, a.Aliases = canonical, append(aliases, redirect)
	return a, nil
}
