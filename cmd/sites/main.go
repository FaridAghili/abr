package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"sites-manager/internal/config"
	"sites-manager/internal/manager"
	"sites-manager/internal/ports"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sites:", err)
		os.Exit(1)
	}
}

const usage = `Usage: sites [--config PATH] [--state-dir DIR] COMMAND [FLAGS]

Commands:
  version          Print build version and platform
  config validate  Validate TOML configuration without host checks
  list             List configured applications
  register         Register an application and reserve required ports
  ports            Show saved reservations (--allocate reconciles config edits)
  doctor           Check config, registry, missing reservations, and listeners

Defaults: --config /etc/sites/config.toml --state-dir /var/lib/sites
Path flags also work after the command. Use COMMAND --help for flags.
Service lifecycle, provisioning, deployment, and the menu are not implemented.
`

func run(args []string, out, stderr io.Writer) error {
	m := manager.Manager{}
	root := flag.NewFlagSet("sites", flag.ContinueOnError)
	root.SetOutput(stderr)
	pathFlags(root, &m, "/etc/sites/config.toml", "/var/lib/sites")
	root.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := root.Parse(args); err != nil {
		return helpError(err)
	}
	args = root.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, usage)
		return nil
	}
	command := args[0]
	args = args[1:]
	if command == "config" {
		if len(args) == 0 || args[0] != "validate" {
			return fmt.Errorf("use sites config validate")
		}
		command, args = "config validate", args[1:]
	}
	switch command {
	case "enable", "disable", "remove", "status", "restart", "logs", "deploy", "setup":
		return fmt.Errorf("%s is not implemented in this milestone", command)
	case "version", "config validate", "list", "register", "ports", "doctor":
	default:
		return fmt.Errorf("unknown command %q; use sites help", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathFlags(fs, &m, m.ConfigPath, m.StateDir)
	var allocate bool
	if command == "ports" {
		fs.BoolVar(&allocate, "allocate", false, "reserve missing endpoints after config edits; keep saved assignments")
	}
	var app config.App
	var driver, mode string
	var imports portFlags
	if command == "register" {
		fs.StringVar(&app.Name, "name", "", "unique application name (required)")
		fs.StringVar(&app.Directory, "dir", "", "absolute application directory (required)")
		fs.StringVar(&app.User, "user", "", "non-root runtime user (required)")
		fs.StringVar(&app.Type, "type", "", "laravel or nuxt (required)")
		fs.StringVar(&app.Domain, "domain", "", "main domain (required)")
		fs.Var((*stringsFlag)(&app.Aliases), "alias", "redirect domain (repeatable)")
		fs.Var((*stringsFlag)(&app.Domains), "serving-domain", "additional serving domain (repeatable)")
		fs.Var((*stringsFlag)(&app.Wildcards), "wildcard", "wildcard domain, e.g. *.example.com (repeatable)")
		fs.StringVar(&app.DeployFile, "deploy-file", "deploy.sh", "project deployment file (stored only)")
		fs.StringVar(&app.HealthCheck, "health-check", "", "optional http(s) URL (stored only)")
		fs.StringVar(&driver, "web-driver", "fpm", "Laravel web driver: fpm or octane")
		fs.IntVar(&app.Web.Workers, "octane-workers", 0, "Octane worker count (0 uses runtime default)")
		fs.IntVar(&app.Queue.Workers, "queue-workers", 0, "Laravel queue worker count")
		fs.BoolVar(&app.Scheduler.Enabled, "scheduler", false, "enable Laravel scheduler")
		fs.BoolVar(&app.Nightwatch.Enabled, "nightwatch", false, "enable Laravel Nightwatch")
		fs.BoolVar(&app.InertiaSSR.Enabled, "inertia-ssr", false, "enable Laravel Inertia SSR")
		fs.StringVar(&mode, "nuxt-mode", "ssr", "Nuxt mode: ssr or static")
		fs.Var(&imports, "port", "import a free port: ENDPOINT=PORT (repeatable)")
	}
	if err := fs.Parse(args); err != nil {
		return helpError(err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s: unexpected arguments: %s", command, strings.Join(fs.Args(), " "))
	}
	if m.ConfigPath == "" || m.StateDir == "" {
		return fmt.Errorf("config and state paths must not be empty")
	}
	switch command {
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
		fmt.Fprintln(w, "NAME\tTYPE\tWEB\tDOMAIN\tDIRECTORY")
		for _, a := range c.Apps {
			web := a.Web.Driver
			if a.Type == "nuxt" {
				web = a.Nuxt.Mode
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.Type, web, a.Domain, a.Directory)
		}
		return w.Flush()
	case "register":
		if app.Type == "laravel" {
			app.Web.Driver = driver
		}
		if app.Type == "nuxt" {
			app.Nuxt.Mode = mode
		}
		var irrelevant string
		fs.Visit(func(f *flag.Flag) {
			if app.Type == "laravel" && f.Name == "nuxt-mode" || app.Type == "nuxt" && f.Name == "web-driver" {
				irrelevant = f.Name
			}
		})
		if irrelevant != "" {
			return fmt.Errorf("--%s is not used by %s apps", irrelevant, app.Type)
		}
		r, err := m.Register(app, imports)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Registered %s\n", app.Name)
		var selected []ports.Assignment
		for _, a := range r.Assignments {
			if a.App == app.Name {
				selected = append(selected, a)
			}
		}
		return printPorts(out, selected)
	case "ports":
		var r ports.Registry
		var err error
		if allocate {
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
	}
	return nil
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
