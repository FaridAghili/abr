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

type DeployOptions struct{ All, NoPull bool }

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
	if name == "git" {
		configured, err := h.sharedGit()
		if err != nil {
			return nil, err
		}
		if configured {
			if err := h.gitAccess(a, false); err != nil {
				return nil, err
			}
			environment = copyEnvironment(environment)
			environment["GIT_SSH_COMMAND"] = h.gitSSH()
			environment["GIT_TERMINAL_PROMPT"] = "0"
		}
	}
	command := []string{"--user", a.User, "--", "env", "-i", "HOME=/var/lib/abr-users/" + a.User, "USER=" + a.User, "LOGNAME=" + a.User, "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin"}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		if key != "PATH" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		command = append(command, key+"="+environment[key])
	}
	command = append(command, name)
	command = append(command, args...)
	return h.run(fmt.Sprintf("Run %s as %s", strings.Join(append([]string{name}, args...), " "), a.User), Command{Name: "runuser", Args: command, Dir: a.Directory, Private: private, Stream: !private || name != "git"})
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
	if len(strings.TrimSpace(string(dirty))) > 0 {
		return fmt.Errorf("working tree is dirty; commit/stash changes before deployment")
	}
	// Disable before pulling/changing code. In-place deployment deliberately has downtime.
	if err := h.disable(a); err != nil {
		return err
	}
	if !o.NoPull {
		if _, err := h.asUser(a, plan.Environment, false, "git", "pull", "--ff-only"); err != nil {
			return err
		}
	}
	sha, err := h.asUser(a, plan.Environment, true, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(string(sha))
	if err := h.permissions(a); err != nil {
		return err
	}
	if a.Database.Enabled {
		if err := h.database(a, false); err != nil {
			return err
		}
	}
	if !h.DryRun {
		if a.Type == "laravel" {
			if err := h.laravelEnv(a); err != nil {
				return err
			}
			if _, err := os.Stat(h.path(filepath.Join(a.Directory, "composer.lock"))); err != nil {
				return fmt.Errorf("commit composer.lock before deployment: %w", err)
			}
		}
	}
	php := "/usr/bin/php" + services.PHPVersion
	run := func(name string, args ...string) error {
		// Artisan/Composer scripts can include SQL bindings and database passwords
		// in exception output. Keep those commands out of terminal/deployment logs.
		_, err := h.asUser(a, plan.Environment, name == php || name == "composer", name, args...)
		return err
	}
	// Build frontend assets before installing PHP dependencies or running Artisan.
	_, packageErr := os.Stat(h.path(filepath.Join(a.Directory, "package.json")))
	if a.Type == "nuxt" || packageErr == nil || h.DryRun {
		if !h.DryRun {
			if _, err := os.Stat(h.path(filepath.Join(a.Directory, "package-lock.json"))); err != nil {
				return fmt.Errorf("commit package-lock.json before deployment: %w", err)
			}
		}
		// Build tools are often dev dependencies even for production builds.
		if err := run("npm", "ci", "--include=dev"); err != nil {
			return err
		}
		if err := run("npm", "run", "build"); err != nil {
			return err
		}
		if err := h.compressAssets(a, plan.Environment); err != nil {
			return err
		}
	} else if !os.IsNotExist(packageErr) {
		return packageErr
	}
	if a.Type == "laravel" {
		if err := run("composer", "install", "--no-dev", "--optimize-autoloader", "--no-interaction", "--prefer-dist"); err != nil {
			return err
		}
		if err := run("composer", "check-platform-reqs", "--no-dev"); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would generate APP_KEY only if missing")
		} else {
			data, err := h.read(filepath.Join(a.Directory, ".env"))
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
	if err := h.enable(a, r); err != nil {
		return err
	}
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
	data, err := h.read(filepath.Join(a.Directory, ".env"))
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
	for key, want := range map[string]string{"DB_CONNECTION": "mysql", "DB_HOST": "localhost", "DB_PORT": "3306", "DB_DATABASE": c.Database, "DB_USERNAME": c.User, "DB_PASSWORD": c.Password} {
		if dotenvValue(data, key) != want {
			return fmt.Errorf("%s: .env %s does not match the managed database; copy values from %s", a.Name, key, h.credentialsEnvPath(a.Name))
		}
	}
	return nil
}
