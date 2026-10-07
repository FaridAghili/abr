package services

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
)

// Exercise the actual header middleware with Caddy's own Via header and headers
// supplied by an upstream. All listeners and files belong to disposable fixtures.
func TestCaddyProxyHeaders(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "fixture-server")
		w.Header().Set("Via", "1.1 fixture-proxy")
		w.Header().Set("X-Powered-By", "fixture-runtime")
		w.Header().Set("Cache-Control", "no-cache, private")
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = io.WriteString(w, "fixture response")
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	upstreamPort, _ := strconv.Atoi(port)
	dir := t.TempDir()
	a := config.App{Name: "app", User: "abr-app", Directory: dir, Type: "nuxt", Domain: "app.test"}
	r := ports.Registry{Version: 1, Assignments: []ports.Assignment{{App: "app", Purpose: "nuxt-http", Port: upstreamPort}}}
	plan, err := Render(a, r, "../../templates", dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	var site string
	for _, file := range plan.Files {
		if strings.HasSuffix(file.Path, ".caddy") {
			// Test the managed serving block over loopback HTTP, without issuing
			// certificates or binding the production HTTP/HTTPS ports.
			site, _, _ = strings.Cut(string(file.Data), "# Explicit HTTP routes")
			site = strings.Replace(site, "app.test {", "http://"+address+" {", 1)
		}
	}
	path := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(path, []byte("{\n admin off\n auto_https off\n}\n"+site), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(caddy, "run", "--config", path, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	client := &http.Client{Timeout: time.Second}
	for _, test := range []struct {
		path string
		code int
	}{{"/", 200}, {"/missing-page", 404}, {"/_nuxt/missing-AbCd1234.css", 404}} {
		deadline := time.Now().Add(5 * time.Second)
		var response *http.Response
		for {
			response, err = client.Get("http://" + address + test.path)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != test.code {
			t.Fatalf("%s: status %d; want %d", test.path, response.StatusCode, test.code)
		}
		for _, name := range []string{"Server", "Via", "X-Powered-By"} {
			if values := response.Header.Values(name); len(values) > 0 {
				t.Errorf("%s: leaked %s: %v", test.path, name, values)
			}
		}
		if test.path == "/" && response.Header.Get("Cache-Control") != "no-cache, private" {
			t.Fatal("changed the application's cache policy")
		}
	}
}

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

// Use Caddy's parser so syntactically invalid directives cannot pass just
// because template rendering succeeded. Ubuntu CI runs this after host setup.
func TestCaddyTemplatesAdapt(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	for _, kind := range []string{"fpm", "octane", "nuxt"} {
		t.Run(kind, func(t *testing.T) {
			a := config.App{
				Name: "app", User: "abr-app", Directory: filepath.Join(t.TempDir(), "app"),
				Type: "laravel", Domain: "app.test", Aliases: []string{"old.test"}, Domains: []string{"extra.test"},
			}
			if kind == "nuxt" {
				a.Type = kind
			} else {
				a.Web.Driver = kind
			}
			r := ports.Empty()
			if err := r.Ensure(a, config.Default().Ports, nil, func(int) error { return nil }); err != nil {
				t.Fatal(err)
			}
			plan, err := Render(a, r, "../../templates", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range plan.Files {
				if !strings.HasSuffix(file.Path, ".caddy") {
					continue
				}
				path := filepath.Join(t.TempDir(), "Caddyfile")
				if err := os.WriteFile(path, file.Data, 0600); err != nil {
					t.Fatal(err)
				}
				// Adapt only: never start a listener, request certificates or
				// connect to PHP-FPM/application processes during this test.
				if output, err := exec.Command(caddy, "adapt", "--config", path, "--adapter", "caddyfile").CombinedOutput(); err != nil {
					t.Fatalf("Caddy rejected %s template: %v\n%s", kind, err, output)
				}
			}
		})
	}
}
