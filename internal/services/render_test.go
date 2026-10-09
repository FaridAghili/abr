package services

import (
	"bufio"
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

// Recent Caddy releases flush compressed SSE responses immediately. Ensure an
// event arrives while the upstream is still running, rather than at its EOF.
func TestCaddyStreaming(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	for _, kind := range []string{"octane", "nuxt"} {
		t.Run(kind, func(t *testing.T) {
			release := make(chan struct{})
			event := "data: " + strings.Repeat("streaming fixture ", 64) + "\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, event+"\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()
			defer close(release)
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
			upstreamPort, _ := strconv.Atoi(port)
			a := config.App{Name: "app", User: "abr-app", Directory: t.TempDir(), Type: "laravel", Domain: "app.test"}
			purpose := "octane-http"
			if kind == "nuxt" {
				a.Type, purpose = "nuxt", "nuxt-http"
			} else {
				a.Web.Driver = "octane"
			}
			r := ports.Empty()
			for i, endpoint := range a.Endpoints() {
				endpointPort := upstreamPort + i + 1
				if endpoint == purpose {
					endpointPort = upstreamPort
				}
				r.Assignments = append(r.Assignments, ports.Assignment{App: a.Name, Purpose: endpoint, Port: endpointPort})
			}
			address := startCaddyFixture(t, caddy, a, r)
			// Go's transport requests gzip and decodes it transparently.
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Get("http://" + address + "/events")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK || !response.Uncompressed {
				t.Fatalf("expected compressed SSE response: status=%d uncompressed=%v", response.StatusCode, response.Uncompressed)
			}
			line, err := bufio.NewReader(response.Body).ReadString('\n')
			if err != nil || line != event {
				t.Fatalf("event did not arrive before upstream EOF: %v", err)
			}
		})
	}
}

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
	address := startCaddyFixture(t, caddy, a, r)
	client := &http.Client{Timeout: time.Second}
	for _, test := range []struct {
		path string
		code int
	}{{"/", 200}, {"/missing-page", 404}, {"/_nuxt/missing-AbCd1234.css", 404}} {
		response, err := client.Get("http://" + address + test.path)
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

func startCaddyFixture(t *testing.T, caddy string, a config.App, r ports.Registry) string {
	t.Helper()
	dir := t.TempDir()
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
	// Production formats generated files before validating and loading them.
	if output, err := exec.Command(caddy, "fmt", "--overwrite", path).CombinedOutput(); err != nil {
		t.Fatalf("Caddy could not format fixture: %v\n%s", err, output)
	}
	cmd := exec.Command(caddy, "run", "--config", path, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/.git")
		if err == nil {
			_ = response.Body.Close()
			return address
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCaddyImageCaching(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = io.WriteString(w, "dynamic response")
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	upstreamPort, _ := strconv.Atoi(port)
	for _, kind := range []string{"fpm", "octane", "nuxt"} {
		t.Run(kind, func(t *testing.T) {
			a := config.App{Name: "app", User: "abr-app", Directory: t.TempDir(), Type: "laravel", Domain: "app.test"}
			root, build := filepath.Join(a.Directory, "public"), "/build/assets"
			purpose := "octane-http"
			if kind == "nuxt" {
				a.Type = kind
				root, build, purpose = filepath.Join(a.Directory, ".output/public"), "/_nuxt", "nuxt-http"
			} else {
				a.Web.Driver = kind
			}
			r := ports.Empty()
			for _, endpoint := range a.Endpoints() {
				endpointPort := upstreamPort
				if endpoint != purpose {
					endpointPort++
				}
				r.Assignments = append(r.Assignments, ports.Assignment{App: a.Name, Purpose: endpoint, Port: endpointPort})
			}
			write := func(path string) {
				t.Helper()
				path = filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("public image fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			paths := map[string]string{}
			for _, ext := range []string{"png", "jpg", "jpeg", "gif", "avif", "webp", "svg", "PNG"} {
				for _, path := range []string{"/images/logo." + ext, build + "/plain." + ext} {
					write(path)
					paths[path] = "public, max-age=2592000"
				}
				if ext != "PNG" {
					path := build + "/logo-AbCd1234." + ext
					write(path)
					paths[path] = "public, max-age=31536000, immutable"
				}
			}
			write("/.git/logo.png")
			write("/.env.logo.png")
			if kind != "nuxt" {
				storage := filepath.Join(a.Directory, "storage/app/public")
				if err := os.MkdirAll(storage, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(storage, filepath.Join(root, "storage")); err != nil {
					t.Fatal(err)
				}
				write("/storage/upload.png")
				paths["/storage/upload.png"] = "public, max-age=2592000"
			}
			address := startCaddyFixture(t, caddy, a, r)
			client := &http.Client{Timeout: time.Second}
			request := func(method, path string, headers map[string]string) *http.Response {
				t.Helper()
				req, err := http.NewRequest(method, "http://"+address+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				for key, value := range headers {
					req.Header.Set(key, value)
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = response.Body.Close() })
				return response
			}
			for path, cache := range paths {
				for _, method := range []string{"GET", "HEAD"} {
					response := request(method, path+"?v=1", nil)
					if response.StatusCode != 200 || response.Header.Get("Cache-Control") != cache {
						t.Fatalf("%s %s: status=%d cache=%q; want %q", method, path, response.StatusCode, response.Header.Get("Cache-Control"), cache)
					}
					if method == "GET" {
						body, _ := io.ReadAll(response.Body)
						if string(body) != "public image fixture" {
							t.Fatalf("image was sent to an application worker: %s", path)
						}
					}
					_ = response.Body.Close()
				}
			}
			image := "/images/logo.png"
			etag := request("HEAD", image, nil).Header.Get("Etag")
			for _, test := range []struct {
				headers map[string]string
				status  int
			}{{map[string]string{"If-None-Match": etag}, 304}, {map[string]string{"Range": "bytes=0-3"}, 206}} {
				response := request("GET", image, test.headers)
				if response.StatusCode != test.status || response.Header.Get("Cache-Control") != "public, max-age=2592000" {
					t.Fatal("conditional/partial image response lost caching", response.StatusCode, response.Header)
				}
			}
			for _, path := range []string{build + "/missing-AbCd1234.png", "/.git/logo.png", "/.env.logo.png"} {
				response := request("GET", path, nil)
				if response.StatusCode != 404 || response.Header.Get("Cache-Control") != "" {
					t.Fatal("missing or private image was publicly cached", path, response.StatusCode, response.Header)
				}
			}
			for _, path := range []string{"/dynamic.png", "/missing.png"} {
				response := request("GET", path, nil)
				if strings.Contains(response.Header.Get("Cache-Control"), "public") || (kind != "fpm" && response.Header.Get("Cache-Control") != "private, no-store") {
					t.Fatal("dynamic image cache policy was overwritten", path, response.Header)
				}
			}
			response := request("POST", image, nil)
			if strings.Contains(response.Header.Get("Cache-Control"), "public") {
				t.Fatal("non-read image request was publicly cached")
			}
		})
	}
}

func TestRenderAllComponents(t *testing.T) {
	a := config.App{Name: "app", User: "abr-app", Directory: "/srv/apps/my app%", Type: "laravel", Domain: "app.test", Aliases: []string{"old.test"}, Domains: []string{"extra.test"},
		Embedding: config.Embedding{Paths: []string{"/banner.html", "/ads/*"}, Origins: []string{"*"}}, Web: config.Web{Driver: "octane"}, Queue: config.Queue{Workers: 2}, Scheduler: config.Component{Enabled: true}, Nightwatch: config.Component{Enabled: true}, InertiaSSR: config.Component{Enabled: true}}
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
				if output, err := exec.Command(caddy, "fmt", "--overwrite", path).CombinedOutput(); err != nil {
					t.Fatalf("Caddy could not format %s template: %v\n%s", kind, err, output)
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

func TestCaddyEmbedding(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "script-src 'self'")
		_, _ = io.WriteString(w, "banner fixture")
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	upstreamPort, _ := strconv.Atoi(port)
	for _, origins := range [][]string{{"*"}, {"self", "https://partner.example.com"}} {
		t.Run(strings.Join(origins, ","), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".output/public/ads"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".output/public/ads/image.png"), []byte("static fixture"), 0644); err != nil {
				t.Fatal(err)
			}
			a := config.App{Name: "app", User: "abr-app", Directory: dir, Type: "nuxt", Domain: "app.test", Embedding: config.Embedding{Paths: []string{"/banner.html", "/ads/*"}, Origins: origins}}
			r := ports.Registry{Version: 1, Assignments: []ports.Assignment{{App: "app", Purpose: "nuxt-http", Port: upstreamPort}}}
			address := startCaddyFixture(t, caddy, a, r)
			client := &http.Client{Timeout: time.Second}
			for _, path := range []string{"/banner.html?campaign=1", "/ads/banner", "/ads/image.png", "/", "/banner.html/extra", "/ads", "/other/banner.html", "/_nuxt/missing.js"} {
				resp, err := client.Get("http://" + address + path)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				embedded := strings.HasPrefix(path, "/banner.html?") || strings.HasPrefix(path, "/ads/")
				if embedded {
					if len(resp.Header.Values("X-Frame-Options")) != 0 {
						t.Errorf("%s: embedding blocked: %v", path, resp.Header)
					}
					found := false
					for _, value := range resp.Header.Values("Content-Security-Policy") {
						if value == a.Embedding.FrameAncestors() {
							found = true
						}
					}
					if !found {
						t.Errorf("%s: missing CSP: %v", path, resp.Header)
					}
				} else if resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
					t.Errorf("%s: lost default protection: %v", path, resp.Header)
				}
				if path == "/banner.html?campaign=1" && !strings.Contains(strings.Join(resp.Header.Values("Content-Security-Policy"), ";"), "script-src 'self'") {
					t.Fatal("lost application's other CSP directives")
				}
			}
			// A separate app with no exception must retain the default.
			a.Embedding = config.Embedding{}
			other := startCaddyFixture(t, caddy, a, r)
			resp, err := client.Get("http://" + other + "/banner.html")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
				t.Fatal("embedding setting leaked to another app")
			}
		})
	}
}
