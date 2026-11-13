package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"abr/internal/config"
	"abr/internal/services"
)

func TestDisableServesMaintenancePageAndRemoveDeletesIt(t *testing.T) {
	h, r, _, a := fixture(t)
	a.Aliases = []string{"www.app.localhost"}
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	site := services.SitePath(a)
	pool := "/etc/php/" + services.PHPVersion + "/fpm/pool.d/abr-app.conf"
	for _, path := range []string{site, pool} {
		if err := h.write(path, []byte(services.Marker+"live\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Files: []string{site, pool}, FPM: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.disable(a); err != nil {
		t.Fatal(err)
	}
	data, err := h.read(site)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{services.Marker, "app.localhost {", "503", "Retry-After", "www.app.localhost {", "redir https://app.localhost{uri} 308"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("maintenance site lacks %q:\n%s", want, data)
		}
	}
	if _, err := os.Lstat(h.path(pool)); !os.IsNotExist(err) {
		t.Fatal("FPM pool kept while disabled")
	}
	reloaded := false
	for _, c := range r.calls {
		if c.Name == "systemctl" && slices.Equal(c.Args, []string{"reload", "caddy"}) {
			reloaded = true
		}
	}
	if !reloaded {
		t.Fatal("Caddy not reloaded with the maintenance site")
	}
	if err := h.Remove(a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(h.path(site)); !os.IsNotExist(err) {
		t.Fatal("removed app kept its site")
	}
}

func TestNuxtInstallsPackagesBeforeStopping(t *testing.T) {
	for _, kind := range []string{"nuxt", "laravel"} {
		t.Run(kind, func(t *testing.T) {
			h, r, _, a := fixture(t)
			a.Type = kind
			if kind == "nuxt" {
				a.Web = config.Web{}
				if err := os.MkdirAll(filepath.Join(a.Directory, ".output/server"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(a.Directory, ".output/server/index.mjs"), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			unit := "abr-app-nuxt.service"
			if kind == "laravel" {
				unit = "abr-app-queue@1.service"
			}
			if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Units: []string{unit}, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			r.fail = func(c Command) error {
				if strings.Contains(strings.Join(c.Args, " "), "npm run build") {
					return testExit(1)
				}
				return nil
			}
			if err := h.Deploy([]string{a.Name}, DeployOptions{NoPull: true}); err == nil {
				t.Fatal("simulated build failure reported success")
			}
			install, stop := -1, -1
			for i, c := range r.calls {
				args := strings.Join(c.Args, " ")
				if c.Name == "runuser" && strings.Contains(args, "npm ci --include=dev") {
					install = i
				}
				if c.Name == "systemctl" && c.Args[0] == "stop" && stop < 0 {
					stop = i
				}
			}
			if install < 0 || stop < 0 {
				t.Fatalf("missing install or stop: %d %d", install, stop)
			}
			if (kind == "nuxt") != (install < stop) {
				t.Fatalf("%s: npm ci at %d, stop at %d", kind, install, stop)
			}
		})
	}
}
