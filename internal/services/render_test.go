package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/config"
	"abr/internal/ports"
)

func TestRenderAllComponents(t *testing.T) {
	a := config.App{Name: "app", User: "abr-app", Directory: "/srv/apps/my app%", Type: "laravel", Domain: "app.test", Aliases: []string{"old.test"}, Domains: []string{"extra.test"}, Web: config.Web{Driver: "octane"}, Queue: config.Queue{Workers: 2}, Scheduler: config.Component{Enabled: true}, Nightwatch: config.Component{Enabled: true}, InertiaSSR: config.Component{Enabled: true}}
	r := ports.Empty()
	if err := r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p, err := Render(a, r, "../../templates", "/var/lib/abr")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 6 || len(p.Files) != 8 {
		t.Fatalf("unexpected plan: %+v", p)
	}
	for _, f := range p.Files {
		text := string(f.Data)
		if strings.Contains(text, "{{") || strings.Contains(text, "Draft") {
			t.Fatalf("unrendered template %s", f.Path)
		}
		if strings.HasSuffix(f.Path, ".service") {
			for _, want := range []string{"User=abr-app\n", "NoNewPrivileges=true\n", "CapabilityBoundingSet=\n", "ProtectSystem=full\n", "ProtectHome=true\n", "PrivateTmp=true\n"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing service protection %q in %s", want, f.Path)
				}
			}
		}
		if strings.HasSuffix(f.Path, ".caddy") {
			for _, want := range []string{"app.test, extra.test", "redir https://app.test{uri} 308", "reverse_proxy 127.0.0.1:10000"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %s in %s", want, text)
				}
			}
		}
		if strings.HasSuffix(f.Path, "octane.service") && (!strings.Contains(text, "WorkingDirectory=/srv/apps/my app%%\n") || !strings.Contains(text, "EnvironmentFile=/var/lib/abr/env/app.env\n") || !strings.Contains(text, "--rpc-host=127.0.0.1 --rpc-port=10001")) {
			t.Fatal(text)
		}
	}
	if p.Environment["NIGHTWATCH_INGEST_URI"] != "127.0.0.1:10003" || p.Environment["SSR_PORT"] != "10002" || p.Environment["APP_DEBUG"] != "false" || p.Environment["OCTANE_HTTPS"] != "true" {
		t.Fatalf("bad environment: %v", p.Environment)
	}
}

func TestNuxtAndFPMTemplates(t *testing.T) {
	for _, kind := range []string{"nuxt", "laravel"} {
		a := config.App{Name: "app", User: "abr-app", Directory: "/srv/apps/app", Type: kind, Domain: "app.test"}
		if kind == "laravel" {
			a.Web.Driver = "fpm"
		}
		r := ports.Empty()
		if err := r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil }); err != nil {
			t.Fatal(err)
		}
		p, err := Render(a, r, "../../templates", "/var/lib/abr")
		if err != nil {
			t.Fatal(err)
		}
		if kind == "nuxt" && (len(r.Assignments) != 1 || len(p.Units) != 1 || p.FPM) {
			t.Fatalf("bad Nuxt plan: %+v", p)
		}
		if kind == "laravel" && (len(r.Assignments) != 0 || len(p.Units) != 0 || !p.FPM) {
			t.Fatalf("bad FPM plan: %+v", p)
		}
		for _, f := range p.Files {
			if strings.HasSuffix(f.Path, ".caddy") && (!strings.Contains(string(f.Data), "@private path /.env /.env.* /.git /.git/*") || !strings.Contains(string(f.Data), "handle @private {\n        respond 404")) {
				t.Fatal("private static files are not blocked", f.Path)
			}
			if strings.HasSuffix(f.Path, "nuxt.service") && !strings.Contains(string(f.Data), "--env-file-if-exists=.env .output/server/index.mjs") {
				t.Fatal(string(f.Data))
			}
			if strings.HasSuffix(f.Path, ".conf") && !strings.HasPrefix(string(f.Data), "; Managed by abr") {
				t.Fatal(string(f.Data))
			}
		}
	}
}

func TestTemplatesAreRequiredAndEditable(t *testing.T) {
	a := config.App{Name: "app", User: "abr-app", Directory: "/srv/apps/app", Type: "nuxt", Domain: "app.test"}
	r := ports.Empty()
	_ = r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil })
	dir := t.TempDir()
	if _, err := Render(a, r, dir, t.TempDir()); err == nil {
		t.Fatal("missing templates accepted")
	}
	for _, name := range []string{"nuxt.service.tmpl", "caddy-site.caddy.tmpl"} {
		data, err := os.ReadFile(filepath.Join("../../templates", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "nuxt.service.tmpl" {
			data = append(data, []byte("\n# owner customization\n")...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Render(a, r, dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.Files[len(p.Files)-1].Data), "owner customization") {
		t.Fatal("edits ignored")
	}
	a.Domain = "*.app.test"
	if _, err := Render(a, r, dir, t.TempDir()); err == nil {
		t.Fatal("wildcard domain accepted")
	}
}

func TestCanonicalRedirectsPreserveURIAndLeaveSubdomainAppsSeparate(t *testing.T) {
	for _, preference := range []string{config.CanonicalWWW, config.CanonicalNonWWW} {
		t.Run(preference, func(t *testing.T) {
			a, err := (config.App{Name: "site", User: "abr-site", Directory: "/srv/apps/site", Type: "laravel", Domain: "example.com", Web: config.Web{Driver: "fpm"}}).WithCanonicalHost(preference)
			if err != nil {
				t.Fatal(err)
			}
			api := config.App{Name: "api", User: "abr-api", Directory: "/srv/apps/api", Type: "laravel", Domain: "api.example.com", Web: config.Web{Driver: "fpm"}}
			for _, app := range []config.App{a, api} {
				plan, err := Render(app, ports.Empty(), "../../templates", t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range plan.Files {
					if !strings.HasSuffix(file.Path, ".caddy") {
						continue
					}
					text := string(file.Data)
					if !strings.Contains(text, "redir https://{host}{uri} 308") || strings.Contains(text, "permanent") {
						t.Fatal("HTTP redirect does not preserve request method")
					}
					if app.Name == "site" {
						if !strings.Contains(text, a.Aliases[0]+" {") || !strings.Contains(text, "http://"+a.Aliases[0]+" {") || !strings.Contains(text, "redir https://"+a.Domain+"{uri} 308") || strings.Contains(text, api.Domain) {
							t.Fatalf("invalid canonical routing: %s", text)
						}
					} else if !strings.Contains(text, "api.example.com {") || strings.Contains(text, "www.") || strings.Contains(text, "redir https://example.com") {
						t.Fatalf("subdomain inherited another app's redirects: %s", text)
					}
				}
			}
		})
	}
}
