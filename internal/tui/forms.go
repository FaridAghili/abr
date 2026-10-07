package tui

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// Each entry has a short explanation and a visible example, even with a default value.
func textInput(title, description, example string, value *string) *huh.Input {
	return huh.NewInput().Title(title).Description(clean(description + "\nExample: " + example)).Placeholder(clean(example)).Value(value)
}

func (m *model) composerAuthForm() tea.Cmd {
	return m.composerCredentialsForm(false)
}

func (m *model) composerCredentialsForm(setup bool) tea.Cmd {
	m.context, m.notice = "", ""
	repository := "nova.laravel.com"
	var username, password string
	return m.setForm("form", "Shared Composer credentials", func() tea.Cmd {
		if m.options.ComposerAuth == nil {
			return m.workflowError("Save Composer credentials", errors.New("Composer credential saving is unavailable"))
		}
		save := m.options.ComposerAuth
		a := action{
			title: "Save shared Composer credentials",
			args:  []string{"composer", "auth", "--host", repository, "--username", username},
			note:  fmt.Sprintf("Repository: %s\nAccount: %s\n\nReuse this account for all app deployments. The token stays hidden.", repository, username),
			run: func(output io.Writer) error {
				defer func() { password = "" }()
				return save(repository, username, password, output)
			},
		}
		if setup {
			a.after = m.setupComplete
			return m.start(a)
		}
		return m.review(a)
	}, huh.NewGroup(
		textInput("Repository hostname", "The package server's hostname, without https:// or a path.", "nova.laravel.com", &repository).Validate(required)),
		huh.NewGroup(textInput("Username / email", "For Nova, enter the email used for your Nova account.", "you@example.com", &username).Validate(required)),
		huh.NewGroup(textInput("Token / license key", "For Nova, paste your license key. Your entry stays hidden.", "your Nova license key", &password).EchoMode(huh.EchoModePassword).Validate(required)))
}

func required(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("Required")
	}
	return nil
}
func applicationName(s string) error {
	if !config.ValidName(s) {
		return errors.New("Use lowercase letters, digits and hyphens; start with a letter (max 63)")
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
		return m.review(action{title: "Set up shared GitHub key", args: args, note: "Create or reuse one GitHub key for this VPS. Apps share its repository access. Add the printed public key to GitHub before cloning."})
	}, huh.NewGroup(textInput("Existing SSH key (optional)", "Leave blank to create or reuse a key. Import an unencrypted private key.", "/root/.ssh/id_ed25519", &key).Validate(func(s string) error {
		if s != "" && (!filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n")) {
			return errors.New("Use an absolute file path, or leave empty")
		}
		return nil
	})))
}

func (m *model) cloneForm() tea.Cmd {
	m.context, m.notice = "", ""
	var repository, name string
	return m.setForm("form", "Clone application", func() tea.Cmd {
		return m.review(action{title: "Clone " + name, args: []string{"clone", repository, name}, note: fmt.Sprintf("Repository: %s\nDirectory: %s\n\nClone, detect Laravel or Nuxt, then continue directly to registration, .env and deployment.", repository, filepath.Join(m.options.AppsDir, name)), after: func() tea.Cmd { return m.clonedRegistration(name) }})
	}, huh.NewGroup(
		textInput("GitHub repository", "Copy the SSH URL from GitHub. Add the server key to GitHub first.", "git@github.com:owner/project.git", &repository).Validate(required)),
		huh.NewGroup(textInput("Application name", "A short name for this app. Its folder is created automatically.", "example-api", &name).Validate(applicationName)))
}

func (m *model) registerForm() tea.Cmd {
	return m.registrationForm("", "", false)
}

func (m *model) registrationForm(name, detected string, cloned bool) tea.Cmd {
	m.context, m.notice = "", ""
	var domain, kind, driver, aliases, domains, health string
	var configOnly, extra bool
	canonicalHost := config.CanonicalWWW
	workers, queue := "2", "0"
	kind, driver = "laravel", "fpm"
	if detected != "" {
		kind = detected
	}
	database := true
	var components []string
	next := func() tea.Cmd {
		args := []string{"register", "--name", name, "--type", kind, "--domain", domain}
		args = append(args, "--canonical-host", canonicalHost)
		if extra && configOnly {
			args = append(args, "--config-only")
		}
		if extra {
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
		}
		if kind == "laravel" {
			args = append(args, "--web-driver", driver, "--queue-workers", queue)
			if driver == "octane" {
				args = append(args, "--octane-workers", workers)
			}
			if extra {
				for _, component := range components {
					args = append(args, "--"+component)
				}
			}
			if !database {
				args = append(args, "--no-database")
			}
		}
		note := "Create the app user and selected database, prepare and review .env when .env.example exists, then deploy this checkout with npm, Composer and migrations as needed."
		if kind == "nuxt" {
			note = "Create the app user, prepare and review .env when .env.example exists, then build with npm and run the Nuxt Node service behind Caddy."
		}
		if extra && configOnly {
			note = "Save configuration and reserve ports for local development only."
		}
		a := action{title: "Register " + name, args: args, note: fmt.Sprintf("Project: %s\nFramework: %s\nDomain: %s\n\n%s", filepath.Join(m.options.AppsDir, name), kind, domain, note)}
		if !(extra && configOnly) {
			a.after = func() tea.Cmd { return m.prepareEnvironment(name) }
		}
		return m.review(a)
	}
	groups := []*huh.Group{huh.NewGroup(huh.NewSelect[string]().Title("Application type").Description("Choose the framework used by this project.\nExample: Laravel for a PHP application.").Options(huh.NewOption("Laravel", "laravel"), huh.NewOption("Nuxt", "nuxt")).Value(&kind)),
		huh.NewGroup(
			textInput("Application name", "Use the name you chose when cloning. The project path is automatic.", "example-api", &name).Validate(applicationName)),
		huh.NewGroup(textInput("Primary domain", "Enter a hostname without https://. Point its DNS to this VPS.", "example.com", &domain).Validate(func(s string) error {
			a := config.App{Name: "test", Directory: "/srv/test", User: "abr-test", Type: "nuxt", Domain: s}
			return a.Validate()
		})),
		huh.NewGroup(huh.NewSelect[string]().Title("Canonical host").Description("Choose which address visitors should use.\nExample: prefer www to redirect example.com to www.example.com.").Options(
			huh.NewOption("Prefer www (default) · redirect non-www", config.CanonicalWWW),
			huh.NewOption("As entered · no automatic www alias", config.CanonicalAsEntered),
			huh.NewOption("Prefer non-www · redirect www", config.CanonicalNonWWW),
		).Value(&canonicalHost))}
	if detected != "nuxt" {
		groups = append(groups,
			huh.NewGroup(huh.NewSelect[string]().Title("Laravel web server").Description("PHP-FPM is the default. Choose Octane if your app uses it.\nExample: PHP-FPM for a standard Laravel app.").Options(huh.NewOption("PHP-FPM (default)", "fpm"), huh.NewOption("Octane / RoadRunner", "octane")).Value(&driver)).WithHideFunc(func() bool { return kind != "laravel" }),
			huh.NewGroup(textInput("Octane workers", "How many Octane processes serve requests. Start with 2.", "2", &workers).Validate(workerCount)).WithHideFunc(func() bool { return kind != "laravel" || driver != "octane" }),
			huh.NewGroup(textInput("Queue workers", "Processes for background jobs. Use 0 if your app has no queue.", "1", &queue).Validate(workerCount)).WithHideFunc(func() bool { return kind != "laravel" }),
			huh.NewGroup(huh.NewSelect[bool]().Title("MySQL database").Description("Create a dedicated database and fill its values into .env.\nExample: enable MySQL for a Laravel app that stores data.").Options(huh.NewOption("Enable MySQL (default)", true), huh.NewOption("Use my own database", false)).Value(&database)).WithHideFunc(func() bool { return kind != "laravel" }),
		)
	}
	groups = append(groups, huh.NewGroup(huh.NewSelect[bool]().Title("Advanced settings?").Description("Most apps can skip these optional settings.\nExample: add an old domain that redirects to your primary domain.").Options(huh.NewOption("Skip (default)", false), huh.NewOption("Configure extras", true)).Value(&extra)))
	if detected != "nuxt" {
		groups = append(groups, huh.NewGroup(huh.NewMultiSelect[string]().Height(8).Title("Laravel components").Description("Optional services used by this project.\nExample: scheduler for scheduled jobs.").Options(huh.NewOption("Scheduler · scheduled jobs", "scheduler"), huh.NewOption("Nightwatch · monitoring", "nightwatch"), huh.NewOption("Inertia SSR · server rendering", "inertia-ssr")).Value(&components)).WithHideFunc(func() bool { return !extra || kind != "laravel" }))
	}
	groups = append(groups,
		huh.NewGroup(textInput("Redirect domains (optional)", "Comma-separated domains to redirect. Leave blank for none.", "old.example.com, legacy.example.com", &aliases)).WithHideFunc(func() bool { return !extra }),
		huh.NewGroup(textInput("Extra serving domains (optional)", "Comma-separated domains that serve this app. Leave blank for none.", "shop.example.com, app.example.com", &domains)).WithHideFunc(func() bool { return !extra }),
		huh.NewGroup(textInput("Health check URL (optional)", "After deployment, check this URL responds successfully. Or leave blank.", "https://example.com/up", &health)).WithHideFunc(func() bool { return !extra }),
		huh.NewGroup(huh.NewSelect[bool]().Title("Registration mode").Description("Manage this VPS, or save settings for local development only.\nExample: manage host when registering an app on your VPS.").Options(huh.NewOption("Manage host user and services", false), huh.NewOption("Config only · portable local development", true)).Value(&configOnly)).WithHideFunc(func() bool { return !extra }),
	)
	if cloned {
		groups = append(groups[:1], groups[2:]...)
	}
	if detected != "" {
		groups = groups[1:]
	}
	return m.setForm("form", "Register application", next, groups...)
}
func (m *model) setupForm() tea.Cmd {
	m.context, m.notice = "", ""
	ssh := "0"
	var admin string
	var hostname string
	var extra bool
	rr := m.options.RoadRunnerVersion
	features := []string{"redis", "images", "firewall"}
	return m.setForm("form", "Set up VPS", func() tea.Cmd {
		if !extra {
			ssh, admin, rr, features = "0", "", m.options.RoadRunnerVersion, []string{"redis", "images", "firewall"}
		}
		args := []string{"setup", "--hostname", hostname, "--ssh-port", ssh, "--roadrunner-version", rr}
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
		return m.review(action{title: "Set up VPS", args: args, note: "Set Ubuntu's hostname, install server tools, configure root's Zsh with Oh My Zsh and both shell plugins, and prepare the TablePlus admin login. Enable security updates and key-only SSH. Then create/reuse and display the GitHub key, ask for Composer credentials and show the TablePlus MySQL login. Test your key login in a second session before continuing.", run: m.setupRunner(args), continueWith: m.setupComposerChoice})
	}, huh.NewGroup(textInput("VPS name", "Sets Ubuntu's hostname and labels the GitHub key. Use lowercase letters, digits and hyphens.", "my-vps", &hostname).Validate(config.ValidateHostname)), huh.NewGroup(huh.NewSelect[bool]().Title("Advanced server settings?").Description("Defaults detect your SSH login and install all shared tools.\nExample: configure a specific SSH port.").Options(huh.NewOption("Skip (default)", false), huh.NewOption("Configure extras", true)).Value(&extra)), huh.NewGroup(textInput("SSH administrator (optional)", "Existing account with a tested SSH key. Blank uses the sudo user or root.", "deploy", &admin)).WithHideFunc(func() bool { return !extra }), huh.NewGroup(textInput("SSH port", "Use 0 to detect current SSH ports, or enter the port you use.", "22", &ssh).Validate(func(s string) error {
		n, e := strconv.Atoi(s)
		if e != nil || n < 0 || n > 65535 {
			return errors.New("Enter 0 or a port from 1 to 65535")
		}
		return nil
	})).WithHideFunc(func() bool { return !extra }), huh.NewGroup(textInput("RoadRunner version", "Use latest for the current stable runtime, or choose a version.", "latest", &rr).Validate(required)).WithHideFunc(func() bool { return !extra }),
		huh.NewGroup(huh.NewMultiSelect[string]().Height(8).Title("Additional tools").Description("Included by default; deselect tools you do not need.\nExample: keep Redis for caching and queues.").Options(huh.NewOption("Redis", "redis"), huh.NewOption("Image-processing tools", "images"), huh.NewOption("Firewall · SSH and HTTPS", "firewall")).Value(&features)).WithHideFunc(func() bool { return !extra }))
}

func (m *model) databaseBackupForm(name string) tea.Cmd {
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		cmd := m.home()
		m.notice = clean(err.Error())
		return cmd
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
		cmd := m.home()
		m.notice = "Register an app with a managed database before backing up."
		return cmd
	}
	directory := filepath.Join(m.options.StateDir, "backups")
	return m.setForm("form", "Back up databases", func() tea.Cmd {
		args := append([]string{"database", "backup", "--output-dir", directory}, selected...)
		return m.review(action{title: "Back up databases", args: args, note: fmt.Sprintf("Directory: %s\nDatabases: %s\n\nExport private SQL backups without downtime. Avoid schema changes until complete.", directory, strings.Join(selected, ", "))})
	}, huh.NewGroup(huh.NewMultiSelect[string]().Title("Databases").Description("Select the databases to export as separate SQL files.\nExample: select your app to back up its database.").Options(choices...).Value(&selected).Validate(func(v []string) error {
		if len(v) == 0 {
			return errors.New("Select at least one database")
		}
		return nil
	})), huh.NewGroup(textInput("Backup directory", "Use a new private directory, or an existing root-owned private one.", filepath.Join(m.options.StateDir, "backups"), &directory).Validate(required)))
}

func (m *model) databaseImportForm(name string) tea.Cmd {
	var path string
	return m.setForm("form", "Import database: "+name, func() tea.Cmd {
		return m.review(action{title: "Import SQL into " + name, args: []string{"database", "import", name, path, "--yes"}, note: fmt.Sprintf("SQL file: %s\n\nThis can overwrite data. Back up and stop app services and other writers first. A failed import can leave partial changes; services stay stopped.", path)})
	}, huh.NewGroup(textInput("SQL file", "Full path to the .sql backup to restore. Back up the current database first.", filepath.Join(m.options.StateDir, "backups", name+".sql"), &path).Validate(func(s string) error {
		if !filepath.IsAbs(s) || strings.ToLower(filepath.Ext(s)) != ".sql" || strings.ContainsAny(s, "\x00\r\n") {
			return errors.New("Use an absolute .sql file path")
		}
		return nil
	})))
}
