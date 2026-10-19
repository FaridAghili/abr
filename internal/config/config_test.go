package config

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func BenchmarkValidateApps(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			c := Default()
			for i := range count {
				a := testApp()
				a.Name = fmt.Sprintf("app-%d", i)
				a.User, a.Directory, a.Domain = a.Name, "/srv/apps/"+a.Name, a.Name+".example.com"
				c.Apps = append(c.Apps, a)
			}
			b.ResetTimer()
			for b.Loop() {
				if err := c.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

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
		{"build order", func(a *App) { a.BuildOrder = "custom" }},
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

func TestBuildOrderValidation(t *testing.T) {
	for _, order := range []string{"", BuildFrontendFirst, BuildComposerFirst} {
		a := testApp()
		a.BuildOrder = order
		if err := a.Validate(); err != nil {
			t.Fatalf("rejected build order %q: %v", order, err)
		}
	}
	a := App{Name: "nuxt", Directory: "/srv/nuxt", User: "nuxt", Type: "nuxt", Domain: "nuxt.test", BuildOrder: BuildComposerFirst}
	if err := a.Validate(); err == nil {
		t.Fatal("accepted Composer build order for Nuxt")
	}
}

func TestAppIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		sharedUser, valid bool
	}{
		{"siblings", "/srv/app", "/srv/app-other", false, true},
		{"nested", "/srv/app", "/srv/app/storage", false, false},
		{"reverse", "/srv/app/storage", "/srv/app", false, false},
		{"cleaned", "/srv/app", "/srv/other/../app/storage", false, false},
		{"root", "/", "/srv/app", false, false},
		{"shared user", "/srv/one", "/srv/two", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := testApp(), testApp()
			a.Directory, b.Directory = tc.left, tc.right
			b.Name, b.User, b.Domain = "other", "other", "other.example.com"
			if tc.sharedUser {
				b.User = a.User
			}
			c := Default()
			c.Apps = []App{a, b}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if !reflect.DeepEqual(c.Apps, []App{a, b}) {
				t.Fatal("validation reordered apps")
			}
		})
	}
}

func TestBackupScheduleValidationAndInterval(t *testing.T) {
	for schedule, interval := range map[string]time.Duration{"": 0, "1h": time.Hour, "6h": 6 * time.Hour, "23h": 23 * time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour} {
		b := Backup{Schedule: schedule, Keep: 3}
		if err := b.Validate(); err != nil || b.Interval() != interval {
			t.Fatal("valid schedule rejected", schedule, err)
		}
	}
	for _, b := range []Backup{{Schedule: "0h"}, {Schedule: "24h"}, {Schedule: "06h"}, {Schedule: "hourly"}, {Schedule: "off"}, {Keep: -1}, {Keep: 1001}} {
		if b.Validate() == nil {
			t.Fatal("invalid backup settings accepted", b)
		}
	}
	a := App{Name: "web", Directory: "/srv/apps/web", User: "abr-web", Type: "nuxt", Domain: "web.example.com", Backup: Backup{Schedule: "6h", Keep: 2}}
	a, err := (AppChanges{Values: App{Backup: Backup{Schedule: "off"}}, Fields: []string{"backup-schedule"}}).Apply(a)
	if err != nil || a.Backup != (Backup{Keep: 2}) {
		t.Fatal("off did not clear only the schedule", a.Backup, err)
	}
}
