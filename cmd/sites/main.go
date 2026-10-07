package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"sites-manager/internal/config"
	"sites-manager/internal/host"
	"sites-manager/internal/manager"
	"sites-manager/internal/ports"
	"sites-manager/internal/tui"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sites:", err)
		os.Exit(1)
	}
}

const usage = `Usage: sites [FLAGS] COMMAND [FLAGS] [APP...]

Commands:
  tui              Interactive application and server menu (default in a terminal)
  version          Print version and platform
  config validate  Validate TOML configuration
  list             List applications
  register         Register a clone, create its Ubuntu user and Laravel database
  ports            Show reservations (--allocate reconciles config edits)
  doctor           Portable config/registry/port checks
  setup            Install shared VPS packages, Caddy, Node 24 and RoadRunner
  git setup        Create/reuse one VPS GitHub SSH key (--key imports an existing key)
  clone URL DIR    Clone a GitHub SSH repository into a new directory under apps-dir
  database APP     Create/verify MySQL database (--show prints credentials)
  database backup APP... | --all --output-dir DIR
                   Export selected/all managed databases as private SQL files
  database import APP FILE.sql --yes
                   Import SQL using the selected database’s scoped account
  enable APP       Render, validate and start services
  disable APP      Stop services; retain users, databases and ports
  remove APP       Remove managed services/user; retain projects and databases
  status [APP]     Show actual service status
  restart APP [SERVICE]  Restart managed services (web, queue, queue@1, etc.)
  logs APP [SERVICE]     Show journal (--follow streams it)
  deploy APP...    Pull, install dependencies, build, migrate, enable
  deploy --all     Deploy sequentially

Paths: --config /etc/sites/config.toml --state-dir /var/lib/sites
       --templates-dir /etc/sites/templates --apps-dir /srv/apps
Host commands require root on Ubuntu 26.04 AMD64; --dry-run previews on macOS.
Portable registration uses --config-only. Without a terminal, no arguments prints help.
`

func run(args []string, out, stderr io.Writer) error {
	m := manager.Manager{}
	h := host.Host{Output: out}
	root := flag.NewFlagSet("sites", flag.ContinueOnError)
	root.SetOutput(stderr)
	pathFlags(root, &m, "/etc/sites/config.toml", "/var/lib/sites")
	hostFlags(root, &h, "/etc/sites/templates", "/srv/apps", false)
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
		if len(args) == 0 || args[0] != "validate" {
			return fmt.Errorf("use sites config validate")
		}
		command, args = "config validate", args[1:]
	}
	if command == "git" {
		if len(args) == 0 || args[0] != "setup" {
			return fmt.Errorf("use sites git setup [--key PATH]")
		}
		command, args = "git setup", args[1:]
	}
	if command == "database" && len(args) > 0 && (args[0] == "backup" || args[0] == "import") {
		command, args = "database "+args[0], args[1:]
	}
	switch command {
	case "tui", "version", "config validate", "list", "register", "ports", "doctor", "setup", "git setup", "clone", "database", "database backup", "database import", "enable", "disable", "remove", "status", "restart", "logs", "deploy":
	default:
		return fmt.Errorf("unknown command %q; use sites help", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathFlags(fs, &m, m.ConfigPath, m.StateDir)
	hostFlags(fs, &h, h.TemplatesDir, h.AppsDir, h.DryRun)
	var allocate, configOnly, noDatabase, show, follow bool
	var app config.App
	var imports portFlags
	var setup host.SetupOptions
	var deploy host.DeployOptions
	var gitKey, backupDirectory string
	var backupAll, importYes bool
	switch command {
	case "git setup":
		fs.StringVar(&gitKey, "key", "", "import an existing unencrypted SSH private key (default: generate/reuse VPS key)")
	case "ports":
		fs.BoolVar(&allocate, "allocate", false, "reserve missing endpoints; retain assignments")
	case "register":
		fs.StringVar(&app.Name, "name", "", "unique application name (required)")
		fs.StringVar(&app.Directory, "dir", "", "absolute cloned project directory (required)")
		fs.StringVar(&app.User, "user", "", "dedicated runtime user (default: sites-NAME)")
		fs.StringVar(&app.Type, "type", "", "laravel or nuxt (required)")
		fs.StringVar(&app.Domain, "domain", "", "main domain (required)")
		fs.Var((*stringsFlag)(&app.Aliases), "alias", "redirect domain (repeatable)")
		fs.Var((*stringsFlag)(&app.Domains), "serving-domain", "additional serving domain (repeatable)")
		fs.StringVar(&app.HealthCheck, "health-check", "", "optional deployment health-check URL")
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
		fs.StringVar(&setup.RoadRunnerVersion, "roadrunner-version", host.DefaultRoadRunnerVersion, "shared RoadRunner release (default: latest stable)")
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
		fs.BoolVar(&show, "show", false, "print existing/generated credentials explicitly")
	case "logs":
		fs.BoolVar(&follow, "follow", false, "stream the journal")
	case "deploy":
		fs.BoolVar(&deploy.All, "all", false, "deploy all apps sequentially")
		fs.BoolVar(&deploy.NoPull, "no-pull", false, "deploy current checkout without git pull")
	}
	if err := fs.Parse(reorderFlags(args, fs)); err != nil {
		return helpError(err)
	}
	positional := fs.Args()
	switch command {
	case "database backup":
		if backupAll == (len(positional) > 0) || backupDirectory == "" {
			return fmt.Errorf("use sites database backup APP... --output-dir DIR, or --all --output-dir DIR")
		}
	case "database import":
		if len(positional) != 2 {
			return fmt.Errorf("use sites database import APP FILE.sql --yes")
		}
	case "clone":
		if len(positional) != 2 {
			return fmt.Errorf("use sites clone git@github.com:OWNER/REPO.git /srv/apps/APP")
		}
	case "enable", "disable", "remove", "database":
		if len(positional) != 1 {
			return fmt.Errorf("use sites %s APP", command)
		}
	case "restart", "logs":
		if len(positional) < 1 || len(positional) > 2 {
			return fmt.Errorf("use sites %s APP [SERVICE]", command)
		}
	case "status":
		if len(positional) > 1 {
			return fmt.Errorf("use sites status [APP]")
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
	case "git setup":
		return h.GitSetup(gitKey)
	case "clone":
		return h.Clone(positional[0], positional[1])
	case "tui":
		if !terminalAvailable(out) {
			return fmt.Errorf("tui requires terminal input and output; use sites help for scriptable commands")
		}
		base := []string{"--config", m.ConfigPath, "--state-dir", m.StateDir,
			"--templates-dir", h.TemplatesDir, "--apps-dir", h.AppsDir,
			"--dry-run=" + strconv.FormatBool(h.DryRun)}
		return tui.Run(tui.Options{
			Version: version, ConfigPath: m.ConfigPath, StateDir: m.StateDir,
			TemplatesDir: h.TemplatesDir, AppsDir: h.AppsDir, DryRun: h.DryRun,
			RoadRunnerVersion: host.DefaultRoadRunnerVersion, Input: os.Stdin, Output: out,
			RunCommand: func(command []string, output io.Writer) error {
				return run(append(append([]string(nil), base...), command...), output, output)
			},
		})
	case "version":
		fmt.Fprintf(out, "sites %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
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
		if err := m.Doctor(); err != nil {
			return err
		}
		fmt.Fprintln(out, "Portable checks passed: config, registry, required reservations, and TCP availability")
	case "setup":
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		setup.DistributionTemplates = filepath.Join(filepath.Dir(executable), "templates")
		if _, err := os.Stat(setup.DistributionTemplates); os.IsNotExist(err) {
			setup.DistributionTemplates = "templates"
		}
		return h.Setup(setup)
	case "database backup":
		return h.BackupDatabases(positional, backupAll, backupDirectory)
	case "database import":
		return h.ImportDatabase(positional[0], positional[1], importYes)
	case "database":
		return h.Database(positional[0], show)
	case "enable":
		return h.Enable(positional[0])
	case "disable":
		return h.Disable(positional[0])
	case "remove":
		return h.Remove(positional[0])
	case "deploy":
		return h.Deploy(positional, deploy)
	case "restart", "logs":
		service := ""
		if len(positional) == 2 {
			service = positional[1]
		}
		if command == "restart" {
			return h.Restart(positional[0], service)
		}
		return h.Logs(positional[0], service, follow)
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
		return fmt.Errorf("expected ENDPOINT=PORT; see README for endpoint names")
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
