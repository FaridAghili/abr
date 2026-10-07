package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalHostSelection(t *testing.T) {
	for _, tt := range []struct {
		name, domain, preference, want string
		aliases                        []string
	}{
		{"default subdomain", "api.example.com", "", "api.example.com", nil},
		{"as entered", "WWW.Example.com", CanonicalAsEntered, "WWW.Example.com", nil},
		{"prefer www", "example.com", CanonicalWWW, "www.example.com", []string{"example.com"}},
		{"already www", "WWW.Example.com", CanonicalWWW, "www.example.com", []string{"example.com"}},
		{"prefer non-www", "www.example.com", CanonicalNonWWW, "example.com", []string{"www.example.com"}},
		{"already non-www", "example.com", CanonicalNonWWW, "example.com", []string{"www.example.com"}},
		{"multi-label suffix", "example.co.uk", CanonicalWWW, "www.example.co.uk", []string{"example.co.uk"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (App{Domain: tt.domain}).WithCanonicalHost(tt.preference)
			if err != nil || got.Domain != tt.want || !reflect.DeepEqual(got.Aliases, tt.aliases) {
				t.Fatalf("domain %q, aliases %v: %v", got.Domain, got.Aliases, err)
			}
		})
	}
}

func TestCanonicalHostPreservesOtherDomainsWithoutMutatingInput(t *testing.T) {
	a := App{Domain: "example.com", Aliases: []string{"old.example.net", "WWW.EXAMPLE.COM", "EXAMPLE.COM", "example.com"}, Domains: []string{"extra.example.net"}}
	wantAliases := append([]string(nil), a.Aliases...)
	got, err := a.WithCanonicalHost(CanonicalWWW)
	if err != nil {
		t.Fatal(err)
	}
	if got.Domain != "www.example.com" || !reflect.DeepEqual(got.Aliases, []string{"old.example.net", "example.com"}) || !reflect.DeepEqual(got.Domains, a.Domains) {
		t.Fatalf("unexpected domains: %+v", got)
	}
	if a.Domain != "example.com" || !reflect.DeepEqual(a.Aliases, wantAliases) {
		t.Fatal("modified input while resolving preference")
	}
	again, err := got.WithCanonicalHost(CanonicalWWW)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("repeated preference changed domains: %v", err)
	}
	reversed, err := got.WithCanonicalHost(CanonicalNonWWW)
	if err != nil || reversed.Domain != "example.com" || !reflect.DeepEqual(reversed.Aliases, []string{"old.example.net", "www.example.com"}) {
		t.Fatalf("reverse preference: %+v, %v", reversed, err)
	}
}

func TestCanonicalHostRejectsInvalidPairsAndServingConflicts(t *testing.T) {
	for _, tt := range []struct {
		app        App
		preference string
	}{
		{App{Domain: "example.com"}, "unexpected"},
		{App{Domain: "https://example.com"}, CanonicalWWW},
		{App{Domain: "*.example.com"}, CanonicalWWW},
		{App{Domain: "www."}, CanonicalNonWWW},
		{App{Domain: strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)}, CanonicalWWW},
		{App{Domain: "example.com", Domains: []string{"EXAMPLE.COM"}}, CanonicalWWW},
		{App{Domain: "example.com", Domains: []string{"www.example.com"}}, CanonicalNonWWW},
	} {
		got, err := tt.app.WithCanonicalHost(tt.preference)
		if err == nil || !reflect.DeepEqual(got, tt.app) {
			t.Fatalf("invalid pair changed app or succeeded: %+v, %v", got, err)
		}
	}
}

func TestCanonicalHostKeepsSubdomainAppsIndependentAndReservesBothHosts(t *testing.T) {
	dir := t.TempDir()
	site, err := (App{Name: "site", Directory: filepath.Join(dir, "site"), User: "abr-site", Type: "nuxt", Domain: "example.com"}).WithCanonicalHost(CanonicalWWW)
	if err != nil {
		t.Fatal(err)
	}
	api := App{Name: "api", Directory: filepath.Join(dir, "api"), User: "abr-api", Type: "nuxt", Domain: "api.example.com"}
	c := Default()
	c.Apps = []App{site, api}
	encoded, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(encoded)
	if err != nil || parsed.Apps[1].Domain != "api.example.com" || len(parsed.Apps[1].Aliases) != 0 || strings.Contains(string(encoded), "canonical_host") {
		t.Fatalf("persisted redundant preference or changed subdomain: %v", err)
	}
	for _, host := range []string{"example.com", "www.example.com"} {
		conflict := c
		conflict.Apps = append(append([]App(nil), c.Apps...), App{Name: "other", Directory: filepath.Join(dir, "other"), User: "abr-other", Type: "nuxt", Domain: host})
		if err := conflict.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate domain") {
			t.Fatalf("hostname %s could be claimed by another app: %v", host, err)
		}
	}
}
