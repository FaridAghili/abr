// Package config describes applications without inspecting the host filesystem.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Ports Range `toml:"ports"`
	Apps  []App `toml:"apps"`
}

type Range struct {
	First int `toml:"first"`
	Last  int `toml:"last"`
}

type App struct {
	Name        string    `toml:"name"`
	Directory   string    `toml:"directory"`
	User        string    `toml:"user"`
	Type        string    `toml:"type"`
	Domain      string    `toml:"domain"`
	Aliases     []string  `toml:"aliases,omitempty"`
	Domains     []string  `toml:"domains,omitempty"`
	HealthCheck string    `toml:"health_check,omitempty"`
	BuildOrder  string    `toml:"build_order,omitempty"`
	Web         Web       `toml:"web,omitempty"`
	Queue       Queue     `toml:"queue,omitempty"`
	Scheduler   Component `toml:"scheduler,omitempty"`
	Nightwatch  Component `toml:"nightwatch,omitempty"`
	InertiaSSR  Component `toml:"inertia_ssr,omitempty"`
	Database    Database  `toml:"database,omitempty"`
}

type Web struct {
	Driver  string `toml:"driver,omitempty"`
	Workers int    `toml:"workers,omitempty"`
}

type Queue struct {
	Workers int `toml:"workers,omitempty"`
}
type Component struct {
	Enabled bool `toml:"enabled,omitempty"`
}
type Database struct {
	Enabled bool `toml:"enabled,omitempty"`
}

const (
	BuildFrontendFirst = "frontend-first"
	BuildComposerFirst = "composer-first"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var userPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

// A VPS name is one DNS label, also suitable for an SSH key comment.
func ValidateHostname(name string) error {
	if !hostnamePattern.MatchString(name) || name == "localhost" {
		return fmt.Errorf("use 1–63 lowercase letters, digits or hyphens; start and end with a letter or digit (example: my-vps)")
	}
	return nil
}

func Default() Config { return Config{Ports: Range{First: 10000, Last: 19999}, Apps: []App{}} }

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(data)
}

func Parse(data []byte) (Config, error) {
	c := Default()
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func Encode(c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return toml.Marshal(c)
}

func (c Config) Validate() error {
	if c.Ports.First < 1024 || c.Ports.Last > 65535 || c.Ports.Last < c.Ports.First {
		return fmt.Errorf("ports: require 1024 <= first <= last <= 65535")
	}
	names, directories, domains := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, a := range c.Apps {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("app %q: %w", a.Name, err)
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate application name %q", a.Name)
		}
		names[a.Name] = true
		dir := filepath.Clean(a.Directory)
		if owner, ok := directories[dir]; ok {
			return fmt.Errorf("apps %q and %q use the same directory", owner, a.Name)
		}
		directories[dir] = a.Name
		all := append([]string{a.Domain}, a.Aliases...)
		all = append(all, a.Domains...)
		for _, d := range all {
			d = strings.ToLower(d)
			if owner, ok := domains[d]; ok {
				return fmt.Errorf("duplicate domain %q in apps %q and %q", d, owner, a.Name)
			}
			domains[d] = a.Name
		}
	}
	// Separate runtime users and directory trees prevent cross-app ownership changes.
	for i, a := range c.Apps {
		for _, b := range c.Apps[i+1:] {
			if a.User == b.User {
				return fmt.Errorf("apps %q and %q share runtime user %q", a.Name, b.Name, a.User)
			}
			left, right := filepath.Clean(a.Directory)+string(filepath.Separator), filepath.Clean(b.Directory)+string(filepath.Separator)
			if strings.HasPrefix(left, right) || strings.HasPrefix(right, left) {
				return fmt.Errorf("apps %q and %q have overlapping directory trees", a.Name, b.Name)
			}
		}
	}
	return nil
}

func (a App) Validate() error {
	if !ValidName(a.Name) {
		return fmt.Errorf("name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens (max 63 characters)")
	}
	if !filepath.IsAbs(a.Directory) || strings.ContainsAny(a.Directory, "\x00\r\n") {
		return fmt.Errorf("directory must be an absolute path")
	}
	if !userPattern.MatchString(a.User) {
		return fmt.Errorf("user must be a valid Unix username")
	}
	if a.User == "root" {
		return fmt.Errorf("runtime user must not be root")
	}
	if !validDomain(a.Domain) {
		return fmt.Errorf("invalid main domain %q", a.Domain)
	}
	for _, d := range append(append([]string{}, a.Aliases...), a.Domains...) {
		if !validDomain(d) {
			return fmt.Errorf("invalid domain %q", d)
		}
	}
	if a.HealthCheck != "" {
		u, err := url.Parse(a.HealthCheck)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("health_check must be an http(s) URL without embedded credentials")
		}
	}
	if a.Queue.Workers < 0 || a.Queue.Workers > 256 || a.Web.Workers < 0 || a.Web.Workers > 256 {
		return fmt.Errorf("worker counts must be between 0 and 256")
	}
	switch a.Type {
	case "laravel":
		if a.BuildOrder != "" && a.BuildOrder != BuildFrontendFirst && a.BuildOrder != BuildComposerFirst {
			return fmt.Errorf("build_order must be frontend-first or composer-first")
		}
		if a.Web.Driver != "fpm" && a.Web.Driver != "octane" {
			return fmt.Errorf("web.driver must be fpm or octane")
		}
		if a.Web.Driver == "fpm" && a.Web.Workers != 0 {
			return fmt.Errorf("web.workers is only used by Octane")
		}
	case "nuxt":
		if a.BuildOrder != "" || a.Web.Driver != "" || a.Web.Workers != 0 || a.Queue.Workers != 0 || a.Scheduler.Enabled || a.Nightwatch.Enabled || a.InertiaSSR.Enabled || a.Database.Enabled {
			return fmt.Errorf("Laravel components cannot be enabled for Nuxt apps")
		}
	default:
		return fmt.Errorf("type must be laravel or nuxt")
	}
	return nil
}

func validDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range strings.ToLower(label) {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

// Endpoints returns only components with TCP listeners, in allocation order.
func (a App) Endpoints() []string {
	var purposes []string
	if a.Type == "laravel" && a.Web.Driver == "octane" {
		purposes = append(purposes, "octane-http", "roadrunner-rpc")
	}
	if a.Type == "nuxt" {
		purposes = append(purposes, "nuxt-http")
	}
	if a.InertiaSSR.Enabled {
		purposes = append(purposes, "inertia-ssr")
	}
	if a.Nightwatch.Enabled {
		purposes = append(purposes, "nightwatch-ingest")
	}
	return purposes
}
