package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

type DeployOptions struct {
	All, NoPull bool
	deferEnable bool
}

func (h Host) Deploy(names []string, o DeployOptions) error {
	if o.All == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
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
		var failures []error
		for _, name := range names {
			a, r, err := h.application(name)
			if err == nil && !h.DryRun {
				r, err = h.Manager.Allocate()
			}
			if err == nil {
				err = h.deploy(a, r, o)
			}
			if err != nil {
				h.say("Deployment failed for %s: %v", name, err)
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
			}
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
	if err := h.project(a); err != nil {
		return err
	}
	if err := h.ensureUser(a); err != nil {
		return err
	}
	if err := h.permissions(a); err != nil {
		return err
	}
	started := time.Now().UTC()
	commit := ""
	// Every deployment has a private log and result, including failed commands.
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
		if h.Output == nil {
			h.Output = f
		} else {
			h.Output = io.MultiWriter(h.Output, f)
		}
		if h.Runner == nil {
			h.Runner = ExecRunner{h.Output}
		}
		h.say("Deployment log: %s.log", logPath)
		defer func() {
			record := struct {
				App, Commit, Started, Finished, Error string
				Success                               bool
			}{App: a.Name, Commit: commit, Started: started.Format(time.RFC3339Nano), Finished: time.Now().UTC().Format(time.RFC3339Nano), Success: result == nil}
			if result != nil {
				record.Error = result.Error()
			}
			data, _ := json.MarshalIndent(record, "", "  ")
			if err := storage.AtomicWrite(h.path(logPath+".json"), data); err != nil {
				result = errors.Join(result, fmt.Errorf("save deployment history: %w", err))
			}
		}()
	}
	dirty, err := h.asUser(a, plan.Environment, true, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	// Fresh restores preserve captured local changes and deploy without pulling.
	if len(strings.TrimSpace(string(dirty))) > 0 && !(o.deferEnable && o.NoPull) {
		return fmt.Errorf("working tree is dirty; commit/stash changes before deployment")
	}
	commit, err = h.deploymentTarget(a, plan.Environment, o.NoPull)
	if err != nil {
		return err
	}
	if err := h.preflight(a, plan.Environment, commit, o.NoPull); err != nil {
		return fmt.Errorf("preflight failed; app services were not stopped: %w", err)
	}
	// In-place deployment deliberately has downtime after preflight succeeds.
	if err := h.disable(a); err != nil {
		return err
	}
	if !o.NoPull {
		if _, err := h.asUser(a, plan.Environment, true, "git", "merge", "--ff-only", commit); err != nil {
			return err
		}
	}
	if err := h.permissions(a); err != nil {
		return err
	}
	php := "/usr/bin/php" + services.PHPVersion
	run := func(name string, args ...string) error {
		// PHP and npm scripts can read .env and include SQL bindings/passwords in
		// exception output. Keep their output out of terminal/deployment logs.
		_, err := h.asUser(a, plan.Environment, name == php || name == "composer" || name == "npm", name, args...)
		if err == nil {
			return nil
		}
		return deploymentCommandError(a.User, name, args, err)
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
		// Build tools are often dev dependencies even for production builds.
		if err := run("npm", "ci", "--include=dev"); err != nil {
			return err
		}
	} else if !os.IsNotExist(packageErr) {
		return packageErr
	}
	installComposer := func() error {
		if err := run("composer", "install", "--no-dev", "--optimize-autoloader", "--no-interaction", "--prefer-dist"); err != nil {
			return err
		}
		return run("composer", "check-platform-reqs", "--no-dev")
	}
	if a.Type == "laravel" && a.BuildOrder == config.BuildComposerFirst {
		if err := installComposer(); err != nil {
			return err
		}
	}
	if frontend {
		if err := run("npm", "run", "build"); err != nil {
			return err
		}
		if err := h.compressAssets(a, plan.Environment); err != nil {
			return err
		}
	}
	if a.Type == "laravel" {
		if a.BuildOrder != config.BuildComposerFirst {
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
		if err := run(php, "artisan", "optimize:clear", "--no-interaction"); err != nil {
			return err
		}
		if err := run(php, "artisan", "optimize", "--no-interaction"); err != nil {
			return err
		}
	}
	if o.deferEnable {
		h.say("Built %s; services remain stopped until full restore completes", a.Name)
		return nil
	}
	if err := h.enable(a, r); err != nil {
		return err
	}
	if err := h.deploymentHealth(a); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Deployment preview complete for %s; no commands executed or files changed", a.Name)
	} else {
		h.say("Deployed %s at %s", a.Name, commit)
	}
	return nil
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

func deploymentCommandError(user, name string, args []string, err error) error {
	var exit interface{ ExitCode() int }
	if name == "composer" && errors.As(err, &exit) && exit.ExitCode() == 100 {
		err = fmt.Errorf("package download failed; for private repositories save credentials with abr composer auth --host HOST: %w", err)
	}
	return fmt.Errorf("%s as %s: %w", strings.Join(append([]string{name}, args...), " "), user, err)
}

func (h Host) deploymentHealth(a config.App) error {
	if a.HealthCheck != "" {
		if h.DryRun {
			h.say("Would check application health at %s", a.HealthCheck)
		} else {
			client := &http.Client{Timeout: 10 * time.Second}
			response, err := client.Get(a.HealthCheck)
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
