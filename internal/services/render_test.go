package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sites-manager/internal/config"
	"sites-manager/internal/ports"
)

func TestRenderAllComponents(t *testing.T) {
	a := config.App{Name: "app", User: "sites-app", Directory: "/srv/apps/my app%", Type: "laravel", Domain: "app.test", Aliases: []string{"old.test"}, Domains: []string{"extra.test"}, Web: config.Web{Driver: "octane"}, Queue: config.Queue{Workers: 2}, Scheduler: config.Component{Enabled: true}, Nightwatch: config.Component{Enabled: true}, InertiaSSR: config.Component{Enabled: true}}
	r := ports.Empty()
	if err := r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p, err := Render(a, r, "../../templates", "/var/lib/sites")
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
		if strings.HasSuffix(f.Path, ".caddy") {
			for _, want := range []string{"app.test, extra.test", "redir https://app.test{uri} permanent", "reverse_proxy 127.0.0.1:10000"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %s in %s", want, text)
				}
			}
		}
		if strings.HasSuffix(f.Path, "octane.service") && (!strings.Contains(text, `WorkingDirectory="/srv/apps/my app%%"`) || !strings.Contains(text, "--rpc-host=127.0.0.1 --rpc-port=10001")) {
			t.Fatal(text)
		}
	}
	if p.Environment["NIGHTWATCH_INGEST_URI"] != "127.0.0.1:10003" || p.Environment["SSR_PORT"] != "10002" || p.Environment["APP_DEBUG"] != "false" || p.Environment["OCTANE_HTTPS"] != "true" {
		t.Fatalf("bad environment: %v", p.Environment)
	}
}

func TestNuxtAndFPMTemplates(t *testing.T) {
	for _, kind := range []string{"nuxt", "laravel"} {
		a := config.App{Name: "app", User: "sites-app", Directory: "/srv/apps/app", Type: kind, Domain: "app.test"}
		if kind == "laravel" {
			a.Web.Driver = "fpm"
		}
		r := ports.Empty()
		if err := r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil }); err != nil {
			t.Fatal(err)
		}
		p, err := Render(a, r, "../../templates", "/var/lib/sites")
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
			if strings.HasSuffix(f.Path, "nuxt.service") && !strings.Contains(string(f.Data), "--env-file-if-exists=.env .output/server/index.mjs") {
				t.Fatal(string(f.Data))
			}
			if strings.HasSuffix(f.Path, ".conf") && !strings.HasPrefix(string(f.Data), "; Managed by sites") {
				t.Fatal(string(f.Data))
			}
		}
	}
}

func TestTemplatesAreRequiredAndEditable(t *testing.T) {
	a := config.App{Name: "app", User: "sites-app", Directory: "/srv/apps/app", Type: "nuxt", Domain: "app.test"}
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
	a.Wildcards = []string{"*.app.test"}
	if _, err := Render(a, r, dir, t.TempDir()); err == nil {
		t.Fatal("unsupported wildcard silently enabled")
	}
}
