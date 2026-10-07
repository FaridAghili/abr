package tui

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func required(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("Required")
	}
	return nil
}
func workerCount(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 256 {
		return errors.New("Enter a count between 0 and 256")
	}
	return nil
}
func (m *model) gitSetupForm() tea.Cmd {
	m.context, m.notice = "", ""
	var key string
	return m.setForm("form", "Shared GitHub key", func() tea.Cmd {
		args := []string{"git", "setup"}
		if key != "" {
			args = append(args, "--key", key)
		}
		return m.review(action{title: "Set up shared GitHub key", args: args, note: "Generate/reuse one VPS SSH key, or import the specified unencrypted private key. Prints the public key to add once to your GitHub account's SSH and GPG keys. All managed apps use this identity and share its repository permissions."})
	}, huh.NewGroup(huh.NewInput().Title("Existing SSH private key path · optional").Description("Leave empty to generate/reuse the VPS key.").Value(&key).Validate(func(s string) error {
		if s != "" && (!filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n")) {
			return errors.New("Use an absolute file path, or leave empty")
		}
		return nil
	})))
}

func (m *model) cloneForm() tea.Cmd {
	m.context, m.notice = "", ""
	var repository, directory string
	return m.setForm("form", "Clone application", func() tea.Cmd {
		return m.review(action{title: "Clone application", args: []string{"clone", repository, directory}, note: "Clone with the shared VPS SSH key. Add its public key to GitHub first. The destination must be new and directly under the apps directory. Register the clone afterward to create its managed user and selected database."})
	}, huh.NewGroup(
		huh.NewInput().Title("GitHub SSH repository URL").Placeholder("git@github.com:OWNER/PROJECT.git").Value(&repository).Validate(required),
		huh.NewInput().Title("New application directory").Placeholder(filepath.Join(m.options.AppsDir, "app")).Value(&directory).Validate(required)))
}

func (m *model) registerForm() tea.Cmd {
	m.context, m.notice = "", ""
	var name, dir, domain, kind, driver, aliases, domains, health string
	var configOnly bool
	canonicalHost := config.CanonicalAsEntered
	workers, queue := "2", "0"
	kind, driver = "laravel", "fpm"
	components := []string{"database"}
	return m.setForm("form", "Register application", func() tea.Cmd {
		args := []string{"register", "--name", name, "--dir", dir, "--type", kind, "--domain", domain}
		args = append(args, "--canonical-host", canonicalHost)
		if configOnly {
			args = append(args, "--config-only")
		}
		for _, item := range []struct{ flag, value string }{{"--alias", aliases}, {"--serving-domain", domains}} {
			for _, d := range strings.Split(item.value, ",") {
				if d = strings.TrimSpace(d); d != "" {
					args = append(args, item.flag, d)
				}
			}
		}
		if health != "" {
			args = append(args, "--health-check", health)
		}
		if kind == "laravel" {
			args = append(args, "--web-driver", driver, "--queue-workers", queue)
			if driver == "octane" {
				args = append(args, "--octane-workers", workers)
			}
			hasDB := false
			for _, component := range components {
				if component == "database" {
					hasDB = true
				} else {
					args = append(args, "--"+component)
				}
			}
			if !hasDB {
				args = append(args, "--no-database")
			}
		}
		note := "Register the existing clone, assign required ports, and create its managed Ubuntu user and selected Laravel database. Registration does not deploy the project."
		if configOnly {
			note = "Save configuration and reserve ports only. No Ubuntu user, database or services are created."
		}
		return m.review(action{title: "Register " + name, args: args, note: note})
	}, huh.NewGroup(huh.NewSelect[string]().Title("Application type").Options(huh.NewOption("Laravel", "laravel"), huh.NewOption("Nuxt · SSR or SPA, managed Node service", "nuxt")).Value(&kind)),
		huh.NewGroup(
			huh.NewInput().Title("Application name").Placeholder("app").Value(&name).Validate(func(s string) error {
				if !config.ValidName(s) {
					return errors.New("Use lowercase letters, digits and hyphens; start with a letter (max 63)")
				}
				return nil
			}),
			huh.NewInput().Title("Existing clone · absolute directory").Placeholder(filepath.Join(m.options.AppsDir, "app")).Value(&dir).Validate(func(s string) error {
				if !filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n") {
					return errors.New("Use an absolute directory path")
				}
				return nil
			}),
			huh.NewInput().Title("Primary domain").Placeholder("example.com").Value(&domain).Validate(func(s string) error {
				a := config.App{Name: "test", Directory: "/srv/test", User: "abr-test", Type: "nuxt", Domain: s}
				return a.Validate()
			})),
		huh.NewGroup(huh.NewSelect[string]().Title("Canonical host").Description("Only this domain and its www counterpart; separate subdomain apps stay independent.").Options(
			huh.NewOption("As entered · no automatic www alias", config.CanonicalAsEntered),
			huh.NewOption("Prefer www · redirect non-www", config.CanonicalWWW),
			huh.NewOption("Prefer non-www · redirect www", config.CanonicalNonWWW),
		).Value(&canonicalHost)),
		huh.NewGroup(huh.NewSelect[string]().Title("Laravel web driver").Options(huh.NewOption("PHP-FPM · Unix socket", "fpm"), huh.NewOption("Octane · shared RoadRunner", "octane")).Value(&driver)).WithHideFunc(func() bool { return kind != "laravel" }),
		huh.NewGroup(huh.NewInput().Title("Octane workers").Value(&workers).Validate(workerCount)).WithHideFunc(func() bool { return kind != "laravel" || driver != "octane" }),
		huh.NewGroup(huh.NewInput().Title("Queue workers · 0 to disable").Value(&queue).Validate(workerCount)).WithHideFunc(func() bool { return kind != "laravel" }),
		huh.NewGroup(huh.NewMultiSelect[string]().Height(8).Title("Laravel components").Description("Space toggles; enter continues.").Options(huh.NewOption("Managed MySQL database", "database").Selected(true), huh.NewOption("Scheduler", "scheduler"), huh.NewOption("Nightwatch", "nightwatch"), huh.NewOption("Inertia SSR", "inertia-ssr")).Value(&components)).WithHideFunc(func() bool { return kind != "laravel" }),
		huh.NewGroup(huh.NewInput().Title("Redirect aliases · comma separated, optional").Value(&aliases), huh.NewInput().Title("Additional serving domains · comma separated, optional").Value(&domains), huh.NewInput().Title("Deployment health-check URL · optional").Value(&health)),
		huh.NewGroup(huh.NewSelect[bool]().Title("Registration mode").Options(huh.NewOption("Manage host user and selected database", false), huh.NewOption("Config only · portable local development", true)).Value(&configOnly)))
}
func (m *model) setupForm() tea.Cmd {
	m.context, m.notice = "", ""
	ssh := "0"
	var admin string
	rr := m.options.RoadRunnerVersion
	features := []string{"redis", "images", "firewall"}
	return m.setForm("form", "Set up VPS", func() tea.Cmd {
		args := []string{"setup", "--ssh-port", ssh, "--roadrunner-version", rr}
		if admin != "" {
			args = append(args, "--admin-user", admin)
		}
		selected := map[string]bool{}
		for _, s := range features {
			selected[s] = true
		}
		for _, f := range []string{"redis", "images", "firewall"} {
			if !selected[f] {
				args = append(args, "--no-"+f)
			}
		}
		return m.review(action{title: "Set up VPS", args: args, note: "Installs shared runtimes and selected tools, configures local MySQL/Redis, and enables security updates and Fail2ban. Applies key-only SSH after checking administrator keys. Preserves the administrator account and SSH port; root key login remains allowed. Test key login in a second session before setup."})
	}, huh.NewGroup(huh.NewInput().Title("Existing SSH admin · optional").Description("Defaults to sudo user or root; install and test its SSH key first.").Value(&admin)), huh.NewGroup(huh.NewInput().Title("SSH port · 0 discovers current sshd ports").Value(&ssh).Validate(func(s string) error {
		n, e := strconv.Atoi(s)
		if e != nil || n < 0 || n > 65535 {
			return errors.New("Enter 0 or a port from 1 to 65535")
		}
		return nil
	}), huh.NewInput().Title("Shared RoadRunner version").Value(&rr).Validate(required)),
		huh.NewGroup(huh.NewMultiSelect[string]().Height(8).Title("Install / configure").Description("Included by default. Space toggles; enter continues.").Options(huh.NewOption("Redis", "redis"), huh.NewOption("Image-processing tools", "images"), huh.NewOption("UFW firewall · preserve SSH, allow HTTP/HTTPS", "firewall")).Value(&features)))
}

func (m *model) databaseBackupForm(name string) tea.Cmd {
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		m.notice = err.Error()
		return m.home()
	}
	choices := []huh.Option[string]{}
	selected := []string{}
	for _, app := range c.Apps {
		if app.Database.Enabled {
			choices = append(choices, huh.NewOption(clean(app.Name), app.Name))
			if name == "" || name == app.Name {
				selected = append(selected, app.Name)
			}
		}
	}
	if len(choices) == 0 {
		m.notice = "No managed databases configured"
		return m.home()
	}
	directory := filepath.Join(m.options.StateDir, "backups")
	return m.setForm("form", "Back up databases", func() tea.Cmd {
		args := append([]string{"database", "backup", "--output-dir", directory}, selected...)
		return m.review(action{title: "Back up databases", args: args, note: "Export each selected managed database into a separate private SQL file. No application downtime. Avoid schema changes during export; consistent snapshots require InnoDB tables. Copy backups off the VPS."})
	}, huh.NewGroup(huh.NewMultiSelect[string]().Title("Databases · space toggles").Options(choices...).Value(&selected).Validate(func(v []string) error {
		if len(v) == 0 {
			return errors.New("Select at least one database")
		}
		return nil
	}), huh.NewInput().Title("Private output directory").Value(&directory).Validate(required)))
}

func (m *model) databaseImportForm(name string) tea.Cmd {
	var path string
	return m.setForm("form", "Import database: "+name, func() tea.Cmd {
		return m.review(action{title: "Import SQL into " + name, args: []string{"database", "import", name, path, "--yes"}, note: "This can overwrite database data and is not transactional. Back up first and disable the app and writers before importing. Uses only this database's scoped account. A failed import can leave partial changes; services are not restarted automatically."})
	}, huh.NewGroup(huh.NewInput().Title("SQL file · absolute path").Value(&path).Validate(func(s string) error {
		if !filepath.IsAbs(s) || strings.ToLower(filepath.Ext(s)) != ".sql" || strings.ContainsAny(s, "\x00\r\n") {
			return errors.New("Use an absolute .sql file path")
		}
		return nil
	})))
}
