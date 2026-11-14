// Package services renders editable files without executing host commands.
package services

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"abr/internal/config"
	"abr/internal/ports"
)

const Marker = "# Managed by abr; edit the source template instead.\n"
const PHPVersion = "8.5"

type File struct {
	Path string
	Data []byte
	Mode os.FileMode
}

type Plan struct {
	Files       []File
	Units       []string // Enabled units; scheduler's oneshot service is not enabled directly.
	FPM         bool
	Environment map[string]string
}

type data struct {
	Name, User, Directory, Type, WebDriver                                string
	SiteDomains, Domain, ReverbDomain                                     string
	Aliases                                                               []string
	EmbedPaths                                                            []string
	FrameAncestors                                                        string
	AssetRoot, AssetPath, HTTPDomains                                     string
	PHPBinary, NodeBinary, ManagedEnvironmentFile, FPMSocket              string
	OctaneHTTPPort, RoadRunnerRPCPort, NuxtHTTPPort, NightwatchIngestPort int
	OctaneWorkers, FPMChildren, QueueWorkers, ReverbPort                  int
	FPMEnvironment                                                        string
}

// SitePath is the Caddy site file of an app, shared by its live and maintenance configurations.
func SitePath(a config.App) string { return "/etc/caddy/abr.d/abr-" + a.Name + ".caddy" }

// render fills one template with the managed marker in front, so every
// generated file identifies itself as abr's.
func render(templates, source string, d data) ([]byte, error) {
	t, err := template.New(source).Option("missingkey=error").Funcs(template.FuncMap{
		"quote": strconv.Quote,
		"unit":  func(s string) string { return strconv.Quote(strings.ReplaceAll(s, "%", "%%")) },
		"unitPath": func(s string) (string, error) {
			// WorkingDirectory/EnvironmentFile are literal paths, not shell
			// words. Quotes are part of the filename for these directives.
			if strings.TrimSpace(s) != s || strings.ContainsAny(s, "\\\x00\r\n") {
				return "", fmt.Errorf("unsupported characters in systemd path")
			}
			return strings.ReplaceAll(s, "%", "%%"), nil
		},
	}).ParseFiles(filepath.Join(templates, source))
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", source, err)
	}
	var b bytes.Buffer
	marker := Marker
	if source == "php-fpm-pool.conf.tmpl" {
		marker = strings.Replace(marker, "#", ";", 1)
	}
	b.WriteString(marker)
	if err := t.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("render %s: %w", source, err)
	}
	return b.Bytes(), nil
}

// RenderMaintenance renders the Caddy site that answers 503 for every domain
// of a stopped app, so visitors get a page and a Retry-After instead of a
// failed TLS handshake while the app is deployed or disabled.
func RenderMaintenance(a config.App, templates string) (File, error) {
	if err := a.Validate(); err != nil {
		return File{}, err
	}
	d := siteData(a)
	data, err := render(templates, "caddy-maintenance.caddy.tmpl", d)
	if err != nil {
		return File{}, err
	}
	return File{SitePath(a), data, 0644}, nil
}

// siteData has the domain fields the maintenance template uses. A stopped
// app's Reverb domain answers 503 too, so WebSocket clients retry later.
func siteData(a config.App) data {
	domains := append([]string{a.Domain}, a.Domains...)
	if a.Reverb.Enabled() {
		domains = append(domains, a.Reverb.Domain)
	}
	d := data{Name: a.Name, Domain: a.Domain, SiteDomains: strings.Join(domains, ", "), Aliases: a.Aliases}
	httpDomains := []string{}
	for _, domain := range domains {
		httpDomains = append(httpDomains, "http://"+domain)
	}
	d.HTTPDomains = strings.Join(httpDomains, ", ")
	return d
}

func Render(a config.App, r ports.Registry, templates, state string) (Plan, error) {
	if err := a.Validate(); err != nil {
		return Plan{}, err
	}
	p := Plan{FPM: a.Type == "laravel" && a.Web.Driver == "fpm", Environment: map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin", "NODE_ENV": "production", "APP_ENV": "production",
	}}
	if a.Type == "laravel" {
		p.Environment["APP_DEBUG"] = "false"
		if a.Web.Driver == "octane" {
			p.Environment["OCTANE_HTTPS"] = "true"
		}
	}
	d := data{Name: a.Name, User: a.User, Directory: a.Directory, Type: a.Type, WebDriver: a.Web.Driver,
		Domain: a.Domain, ReverbDomain: a.Reverb.Domain, SiteDomains: strings.Join(append([]string{a.Domain}, a.Domains...), ", "), Aliases: a.Aliases,
		PHPBinary: "/usr/bin/php" + PHPVersion, NodeBinary: "/usr/bin/node", OctaneWorkers: a.Web.Workers, QueueWorkers: a.Queue.Workers,
		ManagedEnvironmentFile: filepath.Join(state, "env", a.Name+".env"), FPMSocket: "/run/php/abr-" + a.Name + ".sock",
	}
	d.EmbedPaths = a.Embedding.Paths
	d.FrameAncestors = a.Embedding.FrameAncestors()
	d.AssetRoot, d.AssetPath = filepath.Join(a.Directory, "public"), "/build/assets"
	if a.Type == "nuxt" {
		d.AssetRoot, d.AssetPath = filepath.Join(a.Directory, ".output/public"), "/_nuxt"
	}
	httpDomains := []string{}
	for _, domain := range append([]string{a.Domain}, a.Domains...) {
		httpDomains = append(httpDomains, "http://"+domain)
	}
	d.HTTPDomains = strings.Join(httpDomains, ", ")
	// Octane runs its workers permanently; FPM starts children on demand, so a
	// higher ceiling costs memory only while an app is busy.
	d.FPMChildren = a.Web.Workers
	if d.OctaneWorkers == 0 {
		d.OctaneWorkers = 2
	}
	if d.FPMChildren == 0 {
		d.FPMChildren = 8
	}
	for _, purpose := range a.Endpoints() {
		port, ok := r.Lookup(a.Name, purpose)
		if !ok {
			return Plan{}, fmt.Errorf("%s: missing %s reservation; run abr ports --allocate", a.Name, purpose)
		}
		switch purpose {
		case "octane-http":
			d.OctaneHTTPPort = port
		case "roadrunner-rpc":
			d.RoadRunnerRPCPort = port
		case "nuxt-http":
			d.NuxtHTTPPort = port
			p.Environment["NITRO_HOST"] = "127.0.0.1"
			p.Environment["NITRO_PORT"] = strconv.Itoa(port)
		case "nightwatch-ingest":
			d.NightwatchIngestPort = port
			p.Environment["NIGHTWATCH_INGEST_URI"] = "127.0.0.1:" + strconv.Itoa(port)
		case "inertia-ssr":
			p.Environment["INERTIA_SSR_URL"] = "http://127.0.0.1:" + strconv.Itoa(port)
			p.Environment["SSR_PORT"] = strconv.Itoa(port)
		case "reverb":
			// Where Reverb listens. REVERB_HOST and REVERB_PORT stay in .env:
			// Vite builds may copy them into the browser bundle.
			d.ReverbPort = port
			p.Environment["REVERB_SERVER_HOST"] = "127.0.0.1"
			p.Environment["REVERB_SERVER_PORT"] = strconv.Itoa(port)
		}
	}
	keys := make([]string, 0, len(p.Environment))
	for key := range p.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var env strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&env, "%s=%s\n", key, strconv.Quote(p.Environment[key]))
		d.FPMEnvironment += fmt.Sprintf("env[%s] = %s\n", key, strconv.Quote(p.Environment[key]))
	}
	p.Files = append(p.Files, File{d.ManagedEnvironmentFile, []byte(Marker + env.String()), 0600})
	add := func(source, target string) error {
		data, err := render(templates, source, d)
		if err != nil {
			return err
		}
		p.Files = append(p.Files, File{target, data, 0644})
		return nil
	}
	unit := func(source, suffix string, enable bool) error {
		name := "abr-" + a.Name + "-" + suffix
		if err := add(source, filepath.Join("/etc/systemd/system", name)); err != nil {
			return err
		}
		if enable {
			p.Units = append(p.Units, name)
		}
		return nil
	}
	if err := add("caddy-site.caddy.tmpl", SitePath(a)); err != nil {
		return Plan{}, err
	}
	if p.FPM {
		if err := add("php-fpm-pool.conf.tmpl", "/etc/php/"+PHPVersion+"/fpm/pool.d/abr-"+a.Name+".conf"); err != nil {
			return Plan{}, err
		}
	}
	if a.Type == "nuxt" {
		if err := unit("nuxt.service.tmpl", "nuxt.service", true); err != nil {
			return Plan{}, err
		}
	}
	if a.Web.Driver == "octane" {
		if err := unit("octane.service.tmpl", "octane.service", true); err != nil {
			return Plan{}, err
		}
	}
	if a.Queue.Workers > 0 {
		if err := unit("queue-worker.service.tmpl", "queue@.service", false); err != nil {
			return Plan{}, err
		}
		for i := 1; i <= a.Queue.Workers; i++ {
			p.Units = append(p.Units, fmt.Sprintf("abr-%s-queue@%d.service", a.Name, i))
		}
	}
	for _, v := range []struct {
		enabled          bool
		template, suffix string
	}{
		{a.Nightwatch.Enabled, "nightwatch.service.tmpl", "nightwatch.service"},
		{a.InertiaSSR.Enabled, "inertia-ssr.service.tmpl", "inertia-ssr.service"},
		{a.Reverb.Enabled(), "reverb.service.tmpl", "reverb.service"},
		{a.Scheduler.Enabled, "scheduler.service.tmpl", "scheduler.service"},
		{a.Scheduler.Enabled, "scheduler.timer.tmpl", "scheduler.timer"},
	} {
		if v.enabled {
			if err := unit(v.template, v.suffix, v.suffix != "scheduler.service"); err != nil {
				return Plan{}, err
			}
		}
	}
	return p, nil
}
