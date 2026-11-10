package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/services"
	"abr/internal/storage"
	"abr/internal/table"
)

type DeployOptions struct {
	All, NoPull bool
	// Group deploys a group's apps in its order and stops at the first failure.
	Group       string
	deferEnable bool
}

func (h Host) Deploy(names []string, o DeployOptions) error {
	selections := 0
	for _, selected := range []bool{len(names) > 0, o.All, o.Group != ""} {
		if selected {
			selections++
		}
	}
	if selections != 1 {
		return fmt.Errorf("select APP..., --all or --group NAME")
	}
	return h.locked(func() error {
		c, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		if o.All {
			for _, a := range c.Apps {
				names = append(names, a.Name)
			}
		}
		if o.Group != "" {
			g, err := c.Group(o.Group)
			if err != nil {
				return err
			}
			if len(g.Apps) == 0 {
				return fmt.Errorf("group %q has no apps; add apps to it first", o.Group)
			}
			names = g.Apps
		}
		if len(names) == 0 {
			return fmt.Errorf("no applications selected")
		}
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] {
				return fmt.Errorf("duplicate deployment selection %s", name)
			}
			seen[name] = true
			if !slices.ContainsFunc(c.Apps, func(a config.App) bool { return a.Name == name }) {
				return fmt.Errorf("unknown application %q", name)
			}
		}
		summary := table.Table{Title: "Deployment summary", Headers: []string{"APP", "RESULT", "TIME"}, Right: []int{2}}
		if o.Group != "" {
			summary.Title = "Group " + o.Group + " · deployed in order"
		}
		var failures []error
		var skipped []string
		for _, name := range names {
			if o.Group != "" && len(failures) > 0 {
				skipped = append(skipped, name)
				summary.Rows = append(summary.Rows, []table.Cell{table.Styled(name, table.Strong), table.Styled("skipped", table.Muted), table.Styled("—", table.Muted)})
				continue
			}
			began := time.Now()
			a, r, err := h.application(name)
			if err == nil && !h.DryRun {
				r, err = h.Manager.Allocate()
			}
			if err == nil {
				err = h.deploy(a, r, o)
			}
			result := table.Styled("deployed", table.Good)
			if h.DryRun {
				result = table.Styled("previewed", table.Muted)
			}
			if err != nil {
				// A single app's error is reported once, by the caller.
				if len(names) > 1 {
					h.say("Deployment failed for %s: %v", name, err)
				}
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
				result = table.Styled("failed", table.Bad)
			}
			summary.Rows = append(summary.Rows, []table.Cell{table.Styled(name, table.Strong), result, table.Text(time.Since(began).Round(time.Second).String())})
		}
		if len(names) > 1 {
			if err := h.writeTable(summary); err != nil {
				return err
			}
		}
		if len(skipped) > 0 {
			failures = append(failures, fmt.Errorf("group %s stopped at the first failure; not deployed: %s", o.Group, strings.Join(skipped, ", ")))
		}
		return errors.Join(failures...)
	})
}

func (h Host) asUser(a config.App, environment map[string]string, private bool, name string, args ...string) ([]byte, error) {
	if a.User == "" || a.User == "root" {
		return nil, fmt.Errorf("application commands require a dedicated non-root user")
	}
	if name == "composer" {
		_, configured, err := h.sharedComposer()
		if err != nil {
			return nil, err
		}
		if configured {
			if err := h.composerAccess(a, false); err != nil {
				return nil, err
			}
			environment = copyEnvironment(environment)
			environment["COMPOSER_HOME"] = h.composerDir()
			environment["COMPOSER_CACHE_DIR"] = "/var/lib/abr-users/" + a.User + "/.cache/composer"
		}
	}
	if name == "git" {
		environment = copyEnvironment(environment)
		environment["GIT_TERMINAL_PROMPT"] = "0"
		configured, err := h.sharedGit()
		if err != nil {
			return nil, err
		}
		if configured {
			if err := h.gitAccess(a, false); err != nil {
				return nil, err
			}
			environment["GIT_SSH_COMMAND"] = h.gitSSH()
		}
	}
	return h.unprivileged(a.User, "/var/lib/abr-users/"+a.User, a.Directory, environment, private, name, args...)
}

func (h Host) unprivileged(user, home, directory string, environment map[string]string, private bool, name string, args ...string) ([]byte, error) {
	if user == "" || user == "root" {
		return nil, fmt.Errorf("refusing to run application/tool commands as root")
	}
	command := []string{"--user", user, "--", "env", "-i", "HOME=" + home, "USER=" + user, "LOGNAME=" + user, "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin"}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		if key != "PATH" && key != "HOME" && key != "USER" && key != "LOGNAME" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		command = append(command, key+"="+environment[key])
	}
	// Inherited by all dependency scripts and child processes, including execs
	// of setuid programs. runuser alone does not prevent privilege escalation.
	command = append(command, "/usr/bin/setpriv", "--no-new-privs", "--", name)
	command = append(command, args...)
	return h.run(fmt.Sprintf("Run %s as %s", strings.Join(append([]string{name}, args...), " "), user), Command{Name: "runuser", Args: command, Dir: directory, Private: private, Stream: !private || name != "git"})
}

func (h Host) deploy(a config.App, r ports.Registry, o DeployOptions) (result error) {
	plan, err := services.Render(a, r, h.TemplatesDir, h.Manager.StateDir)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	commit := ""
	// Each step's duration goes to the log and history to show where time goes.
	// The terminal gets one line per step that matters to the owner.
	var steps []deployStep
	timed := func(step string, fn func() error) error {
		began := time.Now()
		err := fn()
		if h.DryRun {
			return err
		}
		elapsed := time.Since(began).Seconds()
		steps = append(steps, deployStep{Step: step, Seconds: elapsed})
		label := stepLabels[step]
		if err != nil {
			h.note("  ✗ %s failed", step)
		} else if label != "" {
			h.note("  ✓ %-28s %s", label, formatSeconds(elapsed))
		}
		return err
	}
	// Every deployment has a private log and result, including failed commands.
	// Commands and their output go only to the log.
	if !h.DryRun {
		logPath := filepath.Join(h.Manager.StateDir, "deployments", a.Name, started.Format("20060102T150405.000000000Z"))
		if err := os.MkdirAll(filepath.Dir(h.path(logPath)), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(h.path(logPath+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		h.progress, h.Output = h.Output, f
		if h.Runner == nil {
			h.Runner = ExecRunner{f}
		}
		h.note("Deploying %s", a.Name)
		defer func() {
			record := struct {
				App, Commit, Started, Finished, Error string
				Success                               bool
				Steps                                 []deployStep
			}{App: a.Name, Commit: commit, Started: started.Format(time.RFC3339Nano), Finished: time.Now().UTC().Format(time.RFC3339Nano), Success: result == nil, Steps: steps}
			if result != nil {
				record.Error = result.Error()
			}
			if len(steps) > 0 {
				if err := h.writeTable(stepTable(a.Name, steps)); err != nil {
					result = errors.Join(result, err)
				}
			}
			data, _ := json.MarshalIndent(record, "", "  ")
			if err := storage.AtomicWrite(h.path(logPath+".json"), data); err != nil {
				result = errors.Join(result, fmt.Errorf("save deployment history: %w", err))
			}
			if result != nil {
				result = fmt.Errorf("%w\nFull log: %s.log", result, logPath)
			}
		}()
	}
	if err := h.project(a); err != nil {
		return err
	}
	if err := h.ensureUser(a); err != nil {
		return err
	}
	if err := h.permissions(a); err != nil {
		return err
	}
	dirty, err := h.asUser(a, plan.Environment, true, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	// Restores preserve captured local changes and deploy without pulling, also
	// when a failed restored app is deployed again by hand.
	_, pendingErr := os.Lstat(h.path(h.restorePendingPath(a.Name)))
	if pendingErr != nil && !os.IsNotExist(pendingErr) {
		return pendingErr
	}
	if len(strings.TrimSpace(string(dirty))) > 0 && !(o.NoPull && (o.deferEnable || pendingErr == nil)) {
		return fmt.Errorf("working tree is dirty; commit/stash changes before deployment")
	}
	if err := timed("fetch", func() (err error) {
		commit, err = h.deploymentTarget(a, plan.Environment, o.NoPull)
		return err
	}); err != nil {
		return err
	}
	if err := timed("preflight", func() error { return h.preflight(a, plan.Environment, commit, o.NoPull) }); err != nil {
		return fmt.Errorf("preflight failed; app services were not stopped: %w", err)
	}
	// In-place deployment deliberately has downtime after preflight succeeds.
	if err := timed("stop app", func() error { return h.disable(a) }); err != nil {
		return err
	}
	if !o.NoPull {
		// Git runs as the app user, so the files it writes already belong to it;
		// enable secures ownership and web access again after the build.
		if err := timed("git merge", func() error {
			_, err := h.asUser(a, plan.Environment, true, "git", "merge", "--ff-only", commit)
			return err
		}); err != nil {
			return err
		}
	}
	if a.Type == "laravel" {
		if err := h.useOwnRedis(a, r); err != nil {
			return err
		}
	}
	php := "/usr/bin/php" + services.PHPVersion
	run := func(name string, args ...string) error {
		// PHP and npm scripts can read .env and include SQL bindings/passwords in
		// exception output. Keep their output out of terminal/deployment logs.
		err := timed(stepName(name, args), func() error {
			_, err := h.asUser(a, plan.Environment, name == php || name == "composer" || name == "npm", name, args...)
			return err
		})
		if err == nil {
			return nil
		}
		return deploymentCommandError(a, name, args, err)
	}
	// Frontend-first is the default; apps whose builds invoke Artisan can opt
	// into installing Composer dependencies before the frontend build.
	_, packageErr := os.Stat(h.path(filepath.Join(a.Directory, "package.json")))
	frontend := a.Type == "nuxt" || packageErr == nil || h.DryRun
	if frontend {
		if !h.DryRun {
			if _, err := os.Stat(h.path(filepath.Join(a.Directory, "package-lock.json"))); err != nil {
				return fmt.Errorf("commit package-lock.json before deployment: %w", err)
			}
		}
		if err := h.installAppPackages(a, run); err != nil {
			return err
		}
	} else if !os.IsNotExist(packageErr) {
		return packageErr
	}
	composerFirst := a.BuildOrder == config.BuildComposerFirst
	var composerKey string
	if a.Type == "laravel" && !h.DryRun {
		files, err := h.projectInputs(a, composerInputs)
		if err != nil {
			return err
		}
		composerKey = dependencyKey(h.composerRuntime(), files)
		if err := h.forgetInstall(a, "composer"); err != nil {
			return err
		}
	}
	// A fresh clone or restore has no vendor directory yet, and frontend builds
	// may import from it. Install the packages without Composer scripts first,
	// since frontend-first apps' scripts may need the built assets.
	if a.Type == "laravel" && frontend && !composerFirst {
		if _, err := os.Stat(h.path(filepath.Join(a.Directory, "vendor/autoload.php"))); os.IsNotExist(err) {
			h.note("  No vendor directory yet; installing Composer packages without scripts before the frontend build")
			if err := run("composer", "install", "--no-dev", "--no-scripts", "--no-interaction", "--prefer-dist"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	installComposer := func() error {
		if err := run("composer", "install", "--no-dev", "--optimize-autoloader", "--no-interaction", "--prefer-dist"); err != nil {
			return err
		}
		if err := run("composer", "check-platform-reqs", "--no-dev"); err != nil {
			return err
		}
		// Lets the next preflight skip downloading packages this lock already has.
		if h.DryRun {
			return nil
		}
		return h.rememberInstall(a, "composer", composerKey)
	}
	if a.Type == "laravel" && composerFirst {
		if err := installComposer(); err != nil {
			return err
		}
	}
	if frontend {
		if err := run("npm", "run", "build"); err != nil {
			return err
		}
		if err := timed("brotli", func() error { return h.compressAssets(a, plan.Environment) }); err != nil {
			return err
		}
	}
	if a.Type == "laravel" {
		if !composerFirst {
			if err := installComposer(); err != nil {
				return err
			}
		}
		if h.DryRun {
			h.say("Would generate APP_KEY only if missing")
		} else {
			data, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env")))
			if err != nil {
				return err
			}
			if dotenvValue(data, "APP_KEY") == "" {
				if err := run(php, "artisan", "key:generate", "--force", "--no-interaction"); err != nil {
					return err
				}
			}
		}
		// Clearing the application cache before the first migration fails when
		// Laravel uses its default database cache store and the table is absent.
		if err := run(php, "artisan", "config:clear", "--no-interaction"); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would create public/storage link if absent")
		} else {
			path := h.path(filepath.Join(a.Directory, "public/storage"))
			if info, err := os.Lstat(path); os.IsNotExist(err) {
				if err := run(php, "artisan", "storage:link", "--no-interaction"); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("public/storage exists but is not a symlink")
			}
		}
	}
	if a.Type == "laravel" {
		if err := run(php, "artisan", "migrate", "--force", "--no-interaction"); err != nil {
			return err
		}
		// optimize clears each cache it rebuilds (config, events, routes, views),
		// and the application cache is cleared once the new release serves.
		if err := run(php, "artisan", "optimize", "--no-interaction"); err != nil {
			return err
		}
	}
	if o.deferEnable {
		h.note("Built %s in %s", a.Name, formatSeconds(time.Since(started).Seconds()))
		return nil
	}
	if err := timed("start app", func() error { return h.enable(a, r) }); err != nil {
		return err
	}
	if err := timed("cache:clear", func() error { return h.clearReleasedCache(a, plan.Environment) }); err != nil {
		return err
	}
	if err := timed("health check", func() error { return h.deploymentHealth(a) }); err != nil {
		return err
	}
	if err := h.finishRestore(a.Name); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Deployment preview complete for %s; no commands executed or files changed", a.Name)
	} else {
		h.note("Deployed %s at %.12s in %s", a.Name, commit, formatSeconds(time.Since(started).Seconds()))
	}
	return nil
}

// healthClient sends requests for the app's own hostnames to this server, so
// a deployment or restore checks this server's app even while DNS still
// points elsewhere. Other hosts resolve normally.
func healthClient(a config.App) *http.Client {
	local := map[string]bool{}
	for _, name := range append(append([]string{a.Domain}, a.Domains...), a.Aliases...) {
		local[strings.ToLower(name)] = true
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(address); err == nil && local[strings.ToLower(host)] {
			address = net.JoinHostPort("127.0.0.1", port)
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: transport}
}

// This reads only selected dotenv values for validation, not a general dotenv parser.
func dotenvValue(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && name == key {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func (h Host) laravelEnv(a config.App) error {
	data, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env")))
	if err != nil {
		return fmt.Errorf("prepare the project's .env before deployment: %w", err)
	}
	if !a.Database.Enabled {
		return nil
	}
	saved, err := h.read(h.credentialsPath(a.Name))
	if err != nil {
		return err
	}
	var c credentials
	if err := json.Unmarshal(saved, &c); err != nil {
		return err
	}
	for key, want := range map[string]string{"DB_CONNECTION": "mysql", "DB_HOST": "127.0.0.1", "DB_PORT": "3306", "DB_DATABASE": c.Database, "DB_USERNAME": c.User, "DB_PASSWORD": c.Password} {
		if dotenvValue(data, key) != want {
			return fmt.Errorf("%s: .env %s does not match the managed database; copy values from %s", a.Name, key, h.credentialsEnvPath(a.Name))
		}
	}
	return nil
}

func deploymentCommandError(a config.App, name string, args []string, err error) error {
	var exit interface{ ExitCode() int }
	if name == "composer" && errors.As(err, &exit) && exit.ExitCode() == 100 {
		err = fmt.Errorf("package download failed; for private repositories save credentials with abr composer auth --host HOST: %w", err)
	}
	command := strings.Join(append([]string{name}, args...), " ")
	err = fmt.Errorf("%s as %s: %w", command, a.User, err)
	if name == "npm" || name == "composer" || strings.HasPrefix(filepath.Base(name), "php") {
		// Their output can contain .env secrets, so logs omit it; the owner can
		// rerun the step in a terminal to read it.
		err = fmt.Errorf("%w\nIts output is hidden because it can contain secrets. To see it, run:\n  runuser -u %s -- env -i HOME=/var/lib/abr-users/%s PATH=/usr/local/bin:/usr/bin:/bin bash -c %s", err, a.User, a.User, shellQuote("cd "+a.Directory+" && "+command))
	}
	return err
}

// clearReleasedCache empties a Laravel app's application cache once the new
// release serves requests, so entries that the previous release's PHP workers
// wrote during deployment do not outlive it.
func (h Host) clearReleasedCache(a config.App, environment map[string]string) error {
	if a.Type != "laravel" {
		return nil
	}
	php := "/usr/bin/php" + services.PHPVersion
	args := []string{"artisan", "cache:clear", "--no-interaction"}
	if _, err := h.asUser(a, environment, true, php, args...); err != nil {
		return deploymentCommandError(a, php, args, err)
	}
	return nil
}

func (h Host) deploymentHealth(a config.App) error {
	if a.HealthCheck != "" {
		if h.DryRun {
			h.say("Would check application health at %s", a.HealthCheck)
		} else {
			response, err := healthClient(a).Get(a.HealthCheck)
			if err != nil {
				return fmt.Errorf("health check failed: %w", err)
			}
			response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return fmt.Errorf("health check returned HTTP %d", response.StatusCode)
			}
		}
	}
	return nil
}

// stepLabels names the deployment steps shown in the terminal. Other steps,
// such as clearing caches, appear only in the log and history.
var stepLabels = map[string]string{
	"fetch":            "Fetched code",
	"preflight":        "Checked before stopping",
	"stop app":         "Stopped app",
	"git merge":        "Updated code",
	"npm ci":           "Installed Node packages",
	"npm run build":    "Built frontend",
	"composer install": "Installed Composer packages",
	"artisan migrate":  "Ran migrations",
	"artisan optimize": "Cached config, routes and views",
	"start app":        "Started app",
	"health check":     "Passed health check",
}

type deployStep struct {
	Step    string
	Seconds float64
}

// stepName shortens a command to what identifies the step, such as
// "npm run build" or "artisan migrate", without options or paths.
func stepName(name string, args []string) string {
	words := []string{filepath.Base(name)}
	if strings.HasPrefix(words[0], "php") && len(args) > 0 && args[0] == "artisan" {
		words = nil
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || len(words) == 3 {
			break
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}

func stepTable(app string, steps []deployStep) table.Table {
	t := table.Table{Title: "Step times · " + app, Headers: []string{"STEP", "TIME"}, Right: []int{1}}
	total := 0.0
	for _, s := range steps {
		total += s.Seconds
		t.Rows = append(t.Rows, []table.Cell{table.Text(s.Step), table.Text(formatSeconds(s.Seconds))})
	}
	t.Rows = append(t.Rows, []table.Cell{table.Styled("total", table.Strong), table.Styled(formatSeconds(total), table.Strong)})
	return t
}

func formatSeconds(seconds float64) string {
	if seconds < 10 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}
