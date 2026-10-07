package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"abr/internal/config"
	"abr/internal/services"
)

// Fetch and inspect the exact commit that will be merged, without modifying the
// live checkout. Dependency checks run in an app-owned temporary directory.
func (h Host) deploymentTarget(a config.App, env map[string]string, noPull bool) (string, error) {
	if !noPull {
		if _, err := h.asUser(a, env, true, "git", "fetch", "--prune"); err != nil {
			return "", fmt.Errorf("fetch repository before deployment: %w", err)
		}
	}
	ref := "HEAD"
	if !noPull {
		ref = "@{upstream}^{commit}"
	}
	out, err := h.asUser(a, env, true, "git", "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("resolve deployment commit (configure an upstream branch): %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if h.DryRun {
		return "HEAD", nil
	}
	if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(sha) {
		return "", fmt.Errorf("invalid Git commit identifier")
	}
	if !noPull {
		if _, err := h.asUser(a, env, true, "git", "merge-base", "--is-ancestor", "HEAD", sha); err != nil {
			return "", fmt.Errorf("checkout cannot fast-forward to upstream; reconcile local commits before deployment: %w", err)
		}
	}
	return sha, nil
}

func (h Host) preflight(a config.App, env map[string]string, commit string, noPull bool) error {
	if h.DryRun {
		h.say("Would check .env, dependency manifests/locks, runtime requirements and credential configuration before stopping %s", a.Name)
		return nil
	}
	if a.Type == "laravel" {
		if a.Database.Enabled {
			if err := h.database(a, false); err != nil {
				return err
			}
		}
		if err := h.laravelEnv(a); err != nil {
			return err
		}
		if err := h.checkRoadRunner(a); err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp(h.path(h.AppsDir), ".abr-preflight-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	read := func(name string, optional bool) ([]byte, error) {
		if noPull {
			data, err := readProjectFile(h.path(filepath.Join(a.Directory, name)), maxCommandOutput)
			if optional && os.IsNotExist(err) {
				return nil, nil
			}
			return data, err
		}
		if _, err := h.asUser(a, env, true, "git", "cat-file", "-e", commit+":"+name); err != nil {
			if optional {
				return nil, nil
			}
			return nil, fmt.Errorf("commit %s before deployment", name)
		}
		return h.asUser(a, env, true, "git", "show", commit+":"+name)
	}
	copyFile := func(name string, data []byte) error { return os.WriteFile(filepath.Join(stage, name), data, 0600) }
	for _, name := range []string{"composer.json", "composer.lock"} {
		if a.Type != "laravel" {
			break
		}
		data, err := read(name, false)
		if err != nil {
			return fmt.Errorf("commit %s before deployment: %w", name, err)
		}
		if !json.Valid(data) {
			return fmt.Errorf("%s is not valid JSON", name)
		}
		if err := copyFile(name, data); err != nil {
			return err
		}
	}
	packageJSON, err := read("package.json", a.Type != "nuxt")
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}
	frontend := packageJSON != nil
	if frontend {
		for _, name := range []string{"package.json", "package-lock.json"} {
			data := packageJSON
			if name != "package.json" {
				data, err = read(name, false)
				if err != nil {
					return fmt.Errorf("commit %s before deployment: %w", name, err)
				}
			}
			if !json.Valid(data) {
				return fmt.Errorf("%s is not valid JSON", name)
			}
			if err := copyFile(name, data); err != nil {
				return err
			}
		}
	}
	// Local private auth is not committed. Do not print it or copy the app's .env.
	for _, name := range []string{"auth.json", ".npmrc"} {
		data, err := readProjectFile(h.path(filepath.Join(a.Directory, name)), 1<<20)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read project %s: %w", name, err)
		}
		if err := copyFile(name, data); err != nil {
			return err
		}
	}
	if !noPull && frontend {
		data, err := read(".npmrc", true)
		if err != nil {
			return err
		}
		if data != nil {
			if err := copyFile(".npmrc", data); err != nil {
				return err
			}
		}
	}
	if _, err := h.run("Give the app private access to its preflight directory", Command{Name: "chown", Args: []string{"-R", a.User + ":" + a.User, "--", stage}}); err != nil {
		return err
	}
	candidate := a
	candidate.Directory = stage
	run := func(name string, args ...string) error {
		if _, err := h.asUser(candidate, env, true, name, args...); err != nil {
			return fmt.Errorf("preflight: %w", deploymentCommandError(a.User, name, args, err))
		}
		return nil
	}
	if a.Type == "laravel" {
		if err := run("/usr/bin/php"+services.PHPVersion, "--version"); err != nil {
			return err
		}
		if err := run("composer", "--no-plugins", "validate", "--no-check-publish", "--check-lock", "--no-interaction"); err != nil {
			return err
		}
		if err := run("composer", "--no-plugins", "check-platform-reqs", "--lock", "--no-dev", "--no-interaction"); err != nil {
			return err
		}
		if err := run("composer", "--no-plugins", "install", "--download-only", "--no-dev", "--no-scripts", "--no-interaction", "--prefer-dist"); err != nil {
			return err
		}
	}
	if frontend {
		if err := run("node", "--version"); err != nil {
			return err
		}
		if err := run("npm", "ci", "--dry-run", "--ignore-scripts", "--include=dev", "--engine-strict", "--no-audit", "--no-fund"); err != nil {
			return err
		}
	}
	h.say("Preflight passed for %s", a.Name)
	return nil
}

func (h Host) checkRoadRunner(a config.App) error {
	if a.Web.Driver != "octane" {
		return nil
	}
	info, err := os.Stat(h.path("/usr/local/bin/rr"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("shared RoadRunner is missing or not executable; run abr setup before enabling Octane")
	}
	local := h.path(filepath.Join(a.Directory, "rr"))
	if _, err := os.Lstat(local); err == nil {
		resolved, err := filepath.EvalSymlinks(local)
		shared, sharedErr := filepath.EvalSymlinks(h.path("/usr/local/bin/rr"))
		if err != nil || sharedErr != nil || resolved != shared {
			return fmt.Errorf("%s: app-local rr overrides shared RoadRunner; remove that copy before enabling", a.Name)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
