package config

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

func testApp() App {
	return App{Name: "test", Directory: "/srv/test", User: "test", Type: "laravel", Domain: "test.example.com", Web: Web{Driver: "fpm"}}
}

func TestEndpointSelection(t *testing.T) {
	tests := []struct {
		name string
		app  App
		want []string
	}{
		{"fpm queues scheduler", func() App { a := testApp(); a.Queue.Workers = 2; a.Scheduler.Enabled = true; return a }(), nil},
		{"octane", func() App { a := testApp(); a.Web.Driver = "octane"; return a }(), []string{"octane-http", "roadrunner-rpc"}},
		{"fpm optional", func() App { a := testApp(); a.InertiaSSR.Enabled = true; a.Nightwatch.Enabled = true; return a }(), []string{"inertia-ssr", "nightwatch-ingest"}},
		{"nuxt ssr", App{Name: "nuxt", Directory: "/srv/nuxt", User: "nuxt", Type: "nuxt", Domain: "nuxt.test"}, []string{"nuxt-http"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.app.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := tt.app.Endpoints(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigStrictAndRoundTrip(t *testing.T) {
	data, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	encodedAgain, err := Encode(parsed)
	if err != nil {
		t.Fatal(err)
	}
	// Empty optional arrays may decode as nil after omitempty; serialization
	// must still be stable and preserve the configuration's meaning.
	if !bytes.Equal(encoded, encodedAgain) {
		t.Fatalf("round trip changed config\n%s", encoded)
	}
	for _, bad := range []string{
		"unknown = true", "wildcards = []", "[ports]\nfrist = 1234", "[ports]\nfirst = 20000\nlast = 10000", "[ports]\nfirst = 0", "[[apps]]\nname = 'missing'", string(data) + "\n[apps.nuxt]\nmode = 'ssr'", "[ports]\nfirst = 'not a number'",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted invalid config: %s", bad)
		}
	}
}

func TestDuplicateAndInvalidApps(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*App)
	}{
		{"driver", func(a *App) { a.Web.Driver = "swoole" }},
		{"relative directory", func(a *App) { a.Directory = "srv/app" }},
		{"root", func(a *App) { a.User = "root" }},
		{"domain", func(a *App) { a.Domain = "https://example.com" }},
		{"wildcard", func(a *App) { a.Domain = "*.example.com" }},
		{"negative workers", func(a *App) { a.Queue.Workers = -1 }},
		{"health check credentials", func(a *App) { a.HealthCheck = "https://user:secret@example.com/health" }},
		{"nuxt laravel", func(a *App) { a.Type = "nuxt"; a.Database.Enabled = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := testApp()
			tt.change(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("accepted invalid app")
			}
		})
	}
	c := Default()
	a := testApp()
	c.Apps = []App{a, a}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate application") {
		t.Fatalf("got %v", err)
	}
	b := a
	b.Name, b.Directory = "other", "/srv/other"
	b.Domain = strings.ToUpper(a.Domain)
	c.Apps = []App{a, b}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate domain") {
		t.Fatalf("got %v", err)
	}
	b.Domain, b.Directory = "other.test", a.Directory+"/."
	c.Apps = []App{a, b}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "same directory") {
		t.Fatalf("got %v", err)
	}
}
