package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"abr"
	"abr/internal/config"
	"abr/internal/host"
	"abr/internal/manager"
	"abr/internal/ports"
	"abr/internal/tui"
)

const version = abr.Version

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "abr:", err)
		os.Exit(1)
	}
}

const usage = `Usage: abr [FLAGS] COMMAND [FLAGS] [APP...]

Commands:
  tui              Interactive application and server menu (default in a terminal)
  version          Print version and platform
  config validate  Validate TOML configuration
  config example   Print the bundled example configuration
  list             List applications
  register         Register /srv/apps/NAME, create its Ubuntu user and Laravel database
                   --canonical-host as-entered|www|non-www selects this app's host pair
  edit APP         Save app settings; deploy afterward to apply them
                   --domain HOST --queue-workers N --scheduler=true|false, etc.
  ports            Show reservations (--allocate reconciles config edits)
  doctor           Check configuration, reservations and app TCP listener ownership
  update           Upgrade apt packages, stable Caddy, Composer, global npm tools, Oh My Zsh and shell plugins
  setup            Install shared VPS packages, Caddy, Node 24, RoadRunner and root's Zsh
                   Prompts for VPS name; --hostname NAME is scriptable
  git setup        Create/reuse one VPS GitHub SSH key (--key imports an existing key)
  composer auth    Save shared private-package credentials; prompts for email/token
                   --host HOST --username USER --password-stdin is scriptable
  clone URL NAME   Clone a GitHub SSH repository into /srv/apps/NAME
  env APP          Prepare .env when its example exists; fill managed Laravel MySQL values
  database APP     Create/verify MySQL database (--show prints credentials)
  database --admin TablePlus root connection details (--show prints password)
  database backup APP... | --all --output-dir DIR
                   Export selected/all managed databases as private SQL files
  database import APP FILE.sql --yes
                   Import SQL using the selected database’s scoped account
  enable APP       Render, validate and start services
  disable APP      Stop services; retain users, databases and ports
  remove APP       Remove managed services/user; retain projects and databases
                   --purge --yes permanently deletes this app and its data
  status [APP]     Show actual service status
  disk [APP]       App sizes/totals and live filesystem capacity/used/available space
                   --refresh rescans; --json is scriptable; --filesystem-only skips app scans
  restart APP [SERVICE]  Restart managed services (web, queue, queue@1, etc.)
  logs APP [SERVICE]     Show journal (--follow streams it)
  logs --all            Read all apps' journals in one timeline
                   --type journal|application|deployment --lines N (default 100)
  logs clear APP... | --all --yes
                   Clear file logs (--type all|application|deployment; default all)
  deploy APP...    Pull, install dependencies, build, migrate, enable
  deploy --all     Deploy sequentially
  artisan APP [COMMAND...]  Run Artisan as the Laravel app user (flags pass through)
  shell APP        Open a Laravel app shell; artisan cache:clear; exit to return

Paths: --config /etc/abr/config.toml --state-dir /var/lib/abr
       --templates-dir /etc/abr/templates --apps-dir /srv/apps
Host commands require root on Ubuntu 26.04 AMD64; --dry-run previews on macOS.
Portable registration uses --config-only. Without a terminal, no arguments prints help.
`

func run(args []string, out, stderr io.Writer) error {
	m := manager.Manager{}
	h := host.Host{Output: out}
	root := flag.NewFlagSet("abr", flag.ContinueOnError)
	root.SetOutput(stderr)
	pathFlags(root, &m, "/etc/abr/config.toml", "/var/lib/abr")
	hostFlags(root, &h, "/etc/abr/templates", "/srv/apps", false)
	root.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := root.Parse(args); err != nil {
		return helpError(err)
	}
	args = root.Args()
	if len(args) == 0 && terminalAvailable(out) {
		args = []string{"tui"}
	}
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, usage)
		return nil
	}
	command, args := args[0], args[1:]
	if command == "config" {
		if len(args) == 0 || (args[0] != "validate" && args[0] != "example") {
			return fmt.Errorf("use abr config validate or abr config example")
		}
		command, args = "config "+args[0], args[1:]
	}
	if command == "git" {
		if len(args) == 0 || args[0] != "setup" {
			return fmt.Errorf("use abr git setup [--key PATH]")
		}
		command, args = "git setup", args[1:]
	}
	if command == "composer" {
		if len(args) == 0 || args[0] != "auth" {
			return fmt.Errorf("use abr composer auth --host HOST [--username USER --password-stdin]")
		}
		command, args = "composer auth", args[1:]
	}
	if command == "database" && len(args) > 0 && (args[0] == "backup" || args[0] == "import") {
		command, args = "database "+args[0], args[1:]
	}
	if command == "logs" && len(args) > 0 && args[0] == "clear" {
		command, args = "logs clear", args[1:]
	}
	switch command {
	case "tui", "version", "config validate", "config example", "list", "register", "edit", "ports", "doctor", "setup", "update", "git setup", "composer auth", "clone", "env", "database", "database backup", "database import", "enable", "disable", "remove", "status", "disk", "restart", "logs", "logs clear", "deploy", "artisan", "shell":
	default:
		return fmt.Errorf("unknown command %q; use abr help", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathFlags(fs, &m, m.ConfigPath, m.StateDir)
	hostFlags(fs, &h, h.TemplatesDir, h.AppsDir, h.DryRun)
	var allocate, configOnly, noDatabase, show, databaseAdmin bool
	var app config.App
	var imports portFlags
	var setup host.SetupOptions
	var deploy host.DeployOptions
	var gitKey, backupDirectory string
	var canonicalHost string
	var backupAll, importYes, purge, removeYes bool
	var composerHost, composerUsername string
	var passwordStdin bool
	var changes config.AppChanges
	var clearLogs host.ClearLogsOptions
	var readLogs host.ReadLogsOptions
	var diskUsage host.DiskOptions
	switch command {
	case "disk":
		fs.BoolVar(&diskUsage.All, "all", false, "report all registered apps (default without APP)")
		fs.BoolVar(&diskUsage.Refresh, "refresh", false, "bypass the one-minute app-size cache")
		fs.BoolVar(&diskUsage.JSON, "json", false, "print structured measurements in bytes")
		fs.BoolVar(&diskUsage.FilesystemOnly, "filesystem-only", false, "read live filesystem space without scanning apps or querying MySQL")
	case "edit":
		fs.Var((*stringsFlag)(&app.Embedding.Paths), "embed-path", "replace embeddable paths: /banner.html or /ads/* (repeatable; empty clears)")
		fs.Var((*stringsFlag)(&app.Embedding.Origins), "embed-origin", "replace embedding origins: * for any website, self, or http(s) origin (repeatable; empty clears)")
		fs.StringVar(&app.Domain, "domain", "", "primary domain")
		fs.StringVar(&canonicalHost, "canonical-host", "", "as-entered, www or non-www")
		fs.Var((*stringsFlag)(&app.Aliases), "alias", "replace redirect domains (repeatable; empty clears)")
		fs.Var((*stringsFlag)(&app.Domains), "serving-domain", "replace additional serving domains (repeatable; empty clears)")
		fs.StringVar(&app.HealthCheck, "health-check", "", "deployment health-check URL (empty clears)")
		fs.StringVar(&app.BuildOrder, "build-order", "", "Laravel: frontend-first (default) or composer-first")
		fs.StringVar(&app.Web.Driver, "web-driver", "", "Laravel: fpm or octane")
		fs.IntVar(&app.Web.Workers, "octane-workers", 0, "Octane worker count")
		fs.IntVar(&app.Queue.Workers, "queue-workers", 0, "queue worker count (0 disables)")
		fs.BoolVar(&app.Scheduler.Enabled, "scheduler", false, "enable/disable scheduler")
		fs.BoolVar(&app.Nightwatch.Enabled, "nightwatch", false, "enable/disable Nightwatch")
		fs.BoolVar(&app.InertiaSSR.Enabled, "inertia-ssr", false, "enable/disable Inertia SSR")
		fs.BoolVar(&app.Database.Enabled, "database", false, "enable/disable managed database; existing data retained")
		fs.BoolVar(&configOnly, "config-only", false, "save portable config/ports without host operations")
	case "remove":
		fs.BoolVar(&purge, "purge", false, "permanently delete project, uploads, managed database/account, home, credentials and history")
		fs.BoolVar(&removeYes, "yes", false, "confirm full removal with --purge")
	case "composer auth":
		fs.StringVar(&composerHost, "host", "nova.laravel.com", "private Composer repository hostname")
		fs.StringVar(&composerUsername, "username", "", "repository username/email (default: prompt)")
		fs.BoolVar(&passwordStdin, "password-stdin", false, "read token from standard input instead of a hidden prompt")
	case "git setup":
		fs.StringVar(&gitKey, "key", "", "import an existing unencrypted SSH private key (default: generate/reuse VPS key)")
	case "ports":
		fs.BoolVar(&allocate, "allocate", false, "reserve missing endpoints; retain assignments")
	case "register":
		fs.Var((*stringsFlag)(&app.Embedding.Paths), "embed-path", "embeddable path: /banner.html or /ads/* (repeatable)")
		fs.Var((*stringsFlag)(&app.Embedding.Origins), "embed-origin", "embedding origin: * for any website, self, or http(s) origin (repeatable)")
		fs.StringVar(&app.Name, "name", "", "unique application name (required)")
		fs.StringVar(&app.Directory, "dir", "", "project directory override (default: apps-dir/NAME)")
		fs.StringVar(&app.User, "user", "", "dedicated runtime user (default: abr-NAME)")
		fs.StringVar(&app.Type, "type", "", "laravel or nuxt (required)")
		fs.StringVar(&app.Domain, "domain", "", "main domain (required)")
		fs.StringVar(&canonicalHost, "canonical-host", config.CanonicalAsEntered, "canonical host: as-entered, www or non-www (only this app's www pair)")
		fs.Var((*stringsFlag)(&app.Aliases), "alias", "redirect domain (repeatable)")
		fs.Var((*stringsFlag)(&app.Domains), "serving-domain", "additional serving domain (repeatable)")
		fs.StringVar(&app.HealthCheck, "health-check", "", "optional deployment health-check URL")
		fs.StringVar(&app.BuildOrder, "build-order", "", "Laravel: frontend-first (default) or composer-first")
		fs.StringVar(&app.Web.Driver, "web-driver", "", "Laravel: fpm (default) or octane")
		fs.IntVar(&app.Web.Workers, "octane-workers", 0, "Octane worker count (default 2)")
		fs.IntVar(&app.Queue.Workers, "queue-workers", 0, "Laravel queue worker count")
		fs.BoolVar(&app.Scheduler.Enabled, "scheduler", false, "enable Laravel scheduler")
		fs.BoolVar(&app.Nightwatch.Enabled, "nightwatch", false, "enable Laravel Nightwatch")
		fs.BoolVar(&app.InertiaSSR.Enabled, "inertia-ssr", false, "enable Inertia SSR (bundle must honor SSR_PORT)")
		fs.BoolVar(&configOnly, "config-only", false, "save portable config/ports only; do not create host users/databases")
		fs.BoolVar(&noDatabase, "no-database", false, "Laravel: use an existing/self-managed database")
		fs.Var(&imports, "port", "import free ENDPOINT=PORT (repeatable)")
	case "setup":
		fs.StringVar(&setup.Hostname, "hostname", "", "VPS name: Ubuntu hostname and GitHub key label (default: prompt)")
		fs.StringVar(&setup.RoadRunnerVersion, "roadrunner-version", host.DefaultRoadRunnerVersion, "shared RoadRunner release (default: latest stable in Octane-compatible 2025.1 series)")
		fs.StringVar(&setup.AdminUser, "admin-user", "", "existing SSH administrator (default: sudo user or root); must already have authorized keys")
		fs.IntVar(&setup.SSHPort, "ssh-port", 0, "SSH port to preserve (default: discover effective sshd ports)")
		fs.BoolVar(&setup.NoFirewall, "no-firewall", false, "leave firewall unchanged")
		fs.BoolVar(&setup.NoRedis, "no-redis", false, "skip Redis server")
		fs.BoolVar(&setup.NoImages, "no-images", false, "skip image-processing utilities")
	case "database backup":
		fs.BoolVar(&backupAll, "all", false, "export all registered managed databases")
		fs.StringVar(&backupDirectory, "output-dir", "", "private backup directory (required)")
	case "database import":
		fs.BoolVar(&importYes, "yes", false, "confirm SQL import may overwrite data")
	case "database":
		fs.BoolVar(&databaseAdmin, "admin", false, "show server admin connection details instead of an app database")
		fs.BoolVar(&show, "show", false, "print existing/generated credentials explicitly")
	case "logs":
		fs.BoolVar(&readLogs.Follow, "follow", false, "stream journal logs (CLI only)")
		fs.BoolVar(&readLogs.All, "all", false, "read all registered apps' logs in one view")
		fs.StringVar(&readLogs.Type, "type", "journal", "logs to read: journal, application or deployment")
		fs.IntVar(&readLogs.Lines, "lines", 100, "recent lines per file or combined journal (1-1000; byte limits also apply)")
	case "logs clear":
		fs.BoolVar(&clearLogs.All, "all", false, "clear file logs for all registered apps")
		fs.BoolVar(&clearLogs.Yes, "yes", false, "confirm permanent loss of selected log contents")
		fs.StringVar(&clearLogs.Type, "type", "all", "file logs to clear: all, application or deployment")
	case "deploy":
		fs.BoolVar(&deploy.All, "all", false, "deploy all apps sequentially")
		fs.BoolVar(&deploy.NoPull, "no-pull", false, "deploy current checkout without git pull")
	}
	parseArgs := reorderFlags(args, fs)
	if command == "artisan" {
		// Parse Abr options only before APP. Every argument after APP belongs
		// to Artisan, including --force, --help and flags named like Abr options.
		parseArgs = args
	}
	if err := fs.Parse(parseArgs); err != nil {
		return helpError(err)
	}
	positional := fs.Args()
	if command == "edit" {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "embed-path", "embed-origin", "domain", "canonical-host", "alias", "serving-domain", "health-check", "build-order", "web-driver", "octane-workers", "queue-workers", "scheduler", "nightwatch", "inertia-ssr", "database":
				changes.Fields = append(changes.Fields, f.Name)
			}
		})
		// An explicit empty list flag clears the corresponding setting.
		if len(app.Aliases) == 1 && app.Aliases[0] == "" {
			app.Aliases = nil
		}
		if len(app.Domains) == 1 && app.Domains[0] == "" {
			app.Domains = nil
		}
		for _, list := range []*[]string{&app.Embedding.Paths, &app.Embedding.Origins} {
			if len(*list) == 1 && (*list)[0] == "" {
				*list = nil
			}
		}
		changes.Values, changes.CanonicalHost = app, canonicalHost
		if len(changes.Fields) == 0 {
			return fmt.Errorf("use abr edit APP with settings, for example --queue-workers 2 --scheduler=false")
		}
	}
	switch command {
	case "artisan":
		if len(positional) < 1 {
			return fmt.Errorf("use abr artisan APP [COMMAND...] (Abr flags go before APP)")
		}
	case "logs clear":
		if clearLogs.All == (len(positional) > 0) {
			return fmt.Errorf("use abr logs clear APP... --yes, or abr logs clear --all --yes")
		}
	case "database backup":
		if backupAll == (len(positional) > 0) || backupDirectory == "" {
			return fmt.Errorf("use abr database backup APP... --output-dir DIR, or --all --output-dir DIR")
		}
	case "database import":
		if len(positional) != 2 {
			return fmt.Errorf("use abr database import APP FILE.sql --yes")
		}
	case "clone":
		if len(positional) != 2 {
			return fmt.Errorf("use abr clone git@github.com:OWNER/REPO.git APP")
		}
	case "database":
		if (databaseAdmin && len(positional) != 0) || (!databaseAdmin && len(positional) != 1) {
			return fmt.Errorf("use abr database APP [--show] or abr database --admin [--show]")
		}
	case "enable", "disable", "remove", "edit", "env", "shell":
		if len(positional) != 1 {
			return fmt.Errorf("use abr %s APP", command)
		}
	case "logs":
		if readLogs.Lines < 1 || readLogs.Lines > 1000 {
			return fmt.Errorf("--lines must be between 1 and 1000")
		}
		if (readLogs.All && len(positional) != 0) || (!readLogs.All && (len(positional) < 1 || len(positional) > 2)) {
			return fmt.Errorf("use abr logs APP [SERVICE], or abr logs --all [--type journal|application|deployment]")
		}
	case "restart":
		if len(positional) < 1 || len(positional) > 2 {
			return fmt.Errorf("use abr %s APP [SERVICE]", command)
		}
	case "status", "disk":
		if len(positional) > 1 {
			return fmt.Errorf("use abr %s [APP]", command)
		}
	case "deploy":
	default:
		if len(positional) != 0 {
			return fmt.Errorf("%s: unexpected arguments: %s", command, strings.Join(positional, " "))
		}
	}
	if m.ConfigPath == "" || m.StateDir == "" || h.TemplatesDir == "" || h.AppsDir == "" {
		return fmt.Errorf("paths must not be empty")
	}
	var err error
	for _, path := range []*string{&m.ConfigPath, &m.StateDir, &h.TemplatesDir, &h.AppsDir} {
		*path, err = filepath.Abs(*path)
		if err != nil {
			return err
		}
	}
	h.Manager = m
	switch command {
	case "artisan", "shell":
		if command == "shell" && !h.DryRun && !terminalAvailable(out) {
			return fmt.Errorf("shell requires a terminal; use abr artisan APP COMMAND for scripts")
		}
		process, err := h.LaravelProcess(positional[0], positional[1:], command == "shell")
		if err != nil || process == nil {
			return err
		}
		process.Stdin, process.Stdout, process.Stderr = os.Stdin, out, stderr
		return process.Run()
	case "disk":
		name := ""
		if len(positional) == 1 {
			name = positional[0]
		}
		return h.DiskUsage(name, diskUsage)
	case "edit":
		if configOnly {
			if err := m.Edit(positional[0], changes, !h.DryRun); err != nil {
				return err
			}
			if h.DryRun {
				fmt.Fprintln(out, "Settings preview validated; no files changed")
			} else {
				fmt.Fprintln(out, "Settings saved; deploy on the VPS to apply them")
			}
			return nil
		}
		return h.Edit(positional[0], changes)
	case "composer auth":
		if !h.DryRun {
			if err := host.Require(); err != nil {
				return err
			}
		}
		username, password, err := composerLogin(os.Stdin, stderr, composerUsername, passwordStdin, h.DryRun)
		if err != nil {
			return err
		}
		return h.ComposerAuth(composerHost, username, password)
	case "git setup":
		return h.GitSetup(gitKey)
	case "clone":
		if !config.ValidName(positional[1]) {
			return fmt.Errorf("use an application name: lowercase letters, digits and hyphens; start with a letter (max 63)")
		}
		return h.Clone(positional[0], filepath.Join(h.AppsDir, positional[1]))
	case "tui":
		if !terminalAvailable(out) {
			return fmt.Errorf("tui requires terminal input and output; use abr help for scriptable commands")
		}
		base := []string{"--config", m.ConfigPath, "--state-dir", m.StateDir,
			"--templates-dir", h.TemplatesDir, "--apps-dir", h.AppsDir,
			"--dry-run=" + strconv.FormatBool(h.DryRun)}
		editorHost := h
		editorHost.Output = io.Discard
		return tui.Run(tui.Options{
			Version: version, ConfigPath: m.ConfigPath, StateDir: m.StateDir,
			TemplatesDir: h.TemplatesDir, AppsDir: h.AppsDir, DryRun: h.DryRun,
			RoadRunnerVersion: host.DefaultRoadRunnerVersion, Input: os.Stdin, Output: out,
			RunCommand: func(command []string, output io.Writer) error {
				return run(append(append([]string(nil), base...), command...), output, output)
			},
			EnvEditor: editorHost.EnvEditor,
			ArtisanShell: func(name string) (*exec.Cmd, error) {
				return editorHost.LaravelProcess(name, nil, true)
			},
			ComposerAuth: func(repository, username, password string, output io.Writer) error {
				commandHost := h
				commandHost.Output = output
				return commandHost.ComposerAuth(repository, username, password)
			},
		})
	case "version":
		fmt.Fprintf(out, "abr %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	case "config example":
		data, err := abr.ExampleConfig()
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	case "config validate":
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Configuration valid: %d applications\n", len(c.Apps))
	case "list":
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tWEB\tUSER\tDOMAIN\tDIRECTORY")
		for _, a := range c.Apps {
			web := a.Web.Driver
			if a.Type == "nuxt" {
				web = "node"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Type, web, a.User, a.Domain, a.Directory)
		}
		return w.Flush()
	case "register":
		if app.Directory == "" {
			app.Directory = filepath.Join(h.AppsDir, app.Name)
		}
		app, err = app.WithCanonicalHost(canonicalHost)
		if err != nil {
			return err
		}
		if app.User == "" && config.ValidName(app.Name) {
			app.User = host.RuntimeUser(app.Name)
		}
		if app.Type == "laravel" {
			if app.Web.Driver == "" {
				app.Web.Driver = "fpm"
			}
			app.Database.Enabled = !noDatabase
		}
		if err := app.Validate(); err != nil {
			return err
		}
		var r ports.Registry
		if configOnly {
			if h.DryRun {
				r, err = m.PreviewRegister(app, imports)
			} else {
				r, err = m.Register(app, imports)
			}
			if err == nil {
				if h.DryRun {
					fmt.Fprintf(out, "Would register %s in config only\n", app.Name)
				} else {
					fmt.Fprintf(out, "Registered %s in config only; host user/database not created\n", app.Name)
				}
			}
		} else {
			r, err = h.Register(app, imports)
		}
		if err != nil {
			return err
		}
		var selected []ports.Assignment
		for _, assignment := range r.Assignments {
			if assignment.App == app.Name {
				selected = append(selected, assignment)
			}
		}
		return printPorts(out, selected)
	case "ports":
		var r ports.Registry
		if allocate {
			if h.DryRun {
				return fmt.Errorf("ports --allocate does not support --dry-run")
			}
			r, err = m.Allocate()
		} else {
			r, err = m.Registry()
		}
		if err != nil {
			return err
		}
		return printPorts(out, r.Assignments)
	case "doctor":
		return h.Doctor()
	case "setup":
		name, err := setupHostname(os.Stdin, stderr, setup.Hostname, terminalAvailable(out))
		if err != nil {
			return err
		}
		setup.Hostname = name
		return h.Setup(setup)
	case "update":
		return h.Update()
	case "database backup":
		return h.BackupDatabases(positional, backupAll, backupDirectory)
	case "database import":
		return h.ImportDatabase(positional[0], positional[1], importYes)
	case "database":
		if databaseAdmin {
			return h.MySQLAdmin(show)
		}
		return h.Database(positional[0], show)
	case "env":
		return h.PrepareEnv(positional[0])
	case "enable":
		return h.Enable(positional[0])
	case "disable":
		return h.Disable(positional[0])
	case "remove":
		if purge {
			return h.Purge(positional[0], removeYes)
		}
		if removeYes {
			return fmt.Errorf("--yes is only used with remove --purge")
		}
		return h.Remove(positional[0])
	case "deploy":
		return h.Deploy(positional, deploy)
	case "logs clear":
		return h.ClearLogs(positional, clearLogs)
	case "restart", "logs":
		service := ""
		if len(positional) == 2 {
			service = positional[1]
		}
		if command == "restart" {
			return h.Restart(positional[0], service)
		}
		name := ""
		if len(positional) > 0 {
			name = positional[0]
		}
		return h.ReadLogs(name, service, readLogs)
	case "status":
		if len(positional) == 1 {
			return h.Status(positional[0])
		}
		if !h.DryRun {
			if err := host.Require(); err != nil {
				return err
			}
		}
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		var failures []error
		for _, a := range c.Apps {
			if err := h.Status(a.Name); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	}
	return nil
}

func setupHostname(input io.Reader, output io.Writer, name string, interactive bool) (string, error) {
	if name != "" {
		return name, config.ValidateHostname(name)
	}
	if !interactive {
		return "", fmt.Errorf("use abr setup --hostname NAME outside a terminal (example: my-vps)")
	}
	fmt.Fprintln(output, "VPS name · sets Ubuntu's hostname and labels the GitHub SSH key.")
	fmt.Fprintln(output, "Use lowercase letters, digits and hyphens. Example: my-vps")
	reader := bufio.NewReader(io.LimitReader(input, 4097))
	for {
		fmt.Fprint(output, "VPS name: ")
		line, err := reader.ReadString('\n')
		if err != nil || len(line) > 4096 {
			return "", fmt.Errorf("cannot read VPS name; use --hostname NAME")
		}
		name = strings.TrimSpace(line)
		if err := config.ValidateHostname(name); err != nil {
			fmt.Fprintln(output, err)
			continue
		}
		return name, nil
	}
}

func composerLogin(input io.Reader, output io.Writer, username string, passwordStdin, dryRun bool) (string, string, error) {
	if dryRun {
		return username, "", nil
	}
	if passwordStdin {
		if strings.TrimSpace(username) == "" {
			return "", "", fmt.Errorf("--password-stdin requires --username")
		}
		data, err := io.ReadAll(io.LimitReader(input, 4097))
		if err != nil || len(data) > 4096 {
			return "", "", fmt.Errorf("cannot read Composer token (max 4096 bytes)")
		}
		password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if password == "" || strings.ContainsAny(password, "\x00\r\n") {
			return "", "", fmt.Errorf("Composer token must be one nonempty line")
		}
		return username, password, nil
	}
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", "", fmt.Errorf("use --username USER --password-stdin outside a terminal")
	}
	if username == "" {
		fmt.Fprint(output, "Composer username / email: ")
		line, err := bufio.NewReader(io.LimitReader(input, 4097)).ReadString('\n')
		if err != nil || len(line) > 4096 {
			return "", "", fmt.Errorf("cannot read Composer username")
		}
		username = strings.TrimSpace(line)
	}
	fmt.Fprint(output, "Composer token / license key (hidden): ")
	data, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(output)
	if err != nil {
		return "", "", fmt.Errorf("cannot read Composer token")
	}
	return username, string(data), nil
}

func terminalAvailable(out io.Writer) bool {
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
}

func hostFlags(fs *flag.FlagSet, h *host.Host, templates, apps string, dryRun bool) {
	fs.StringVar(&h.TemplatesDir, "templates-dir", templates, "standalone template directory")
	fs.StringVar(&h.AppsDir, "apps-dir", apps, "dedicated parent directory for project clones")
	fs.BoolVar(&h.DryRun, "dry-run", dryRun, "preview host operations without commands or writes")
}

// Go's flag parser stops at the first positional argument. Permit APP --flags too.
func reorderFlags(args []string, fs *flag.FlagSet) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && boolFlag.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(append(flags, "--"), positional...)
}

func helpError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func pathFlags(fs *flag.FlagSet, m *manager.Manager, configPath, stateDir string) {
	fs.StringVar(&m.ConfigPath, "config", configPath, "TOML configuration file")
	fs.StringVar(&m.StateDir, "state-dir", stateDir, "registry and lock directory")
}

func printPorts(out io.Writer, assignments []ports.Assignment) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "APP\tENDPOINT\tPORT")
	for _, a := range assignments {
		fmt.Fprintf(w, "%s\t%s\t%d\n", a.App, a.Purpose, a.Port)
	}
	return w.Flush()
}

type stringsFlag []string

func (s *stringsFlag) String() string         { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(value string) error { *s = append(*s, value); return nil }

type portFlags map[string]int

func (p *portFlags) String() string { return "ENDPOINT=PORT" }
func (p *portFlags) Set(value string) error {
	parts := strings.Split(value, "=")
	if len(parts) != 2 || !ports.ValidPurpose(parts[0]) {
		return fmt.Errorf("expected ENDPOINT=PORT (octane-http, roadrunner-rpc, nuxt-http, inertia-ssr, nightwatch-ingest)")
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port < 1024 || port > 65535 {
		return fmt.Errorf("port must be between 1024 and 65535")
	}
	if *p == nil {
		*p = portFlags{}
	}
	if _, ok := (*p)[parts[0]]; ok {
		return fmt.Errorf("duplicate endpoint %s", parts[0])
	}
	(*p)[parts[0]] = port
	return nil
}
