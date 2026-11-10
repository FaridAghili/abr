package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"abr/internal/config"
	"abr/internal/services"
)

// npmInstallArgs installs exactly the lock file. Build tools are often dev
// dependencies even for production builds. The lock's integrity hashes make
// cached packages safe to reuse without asking the registry again, and audit
// and funding requests only add network round trips.
var npmInstallArgs = []string{"ci", "--include=dev", "--no-audit", "--no-fund", "--prefer-offline"}

// Files whose contents decide what a dependency install produces. Credentials
// files are included because they can change which registry serves a package.
var (
	npmInputs      = []string{"package.json", "package-lock.json", ".npmrc"}
	composerInputs = []string{"composer.json", "composer.lock", "auth.json"}
)

// dependencyKey identifies one dependency install: the inputs' contents and
// the runtime that installed them. Only the hash is stored, never the files.
func dependencyKey(runtime string, files map[string][]byte) string {
	sum := sha256.New()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(sum, "%s\x00%d\x00", name, len(files[name]))
		sum.Write(files[name])
	}
	fmt.Fprintf(sum, "runtime\x00%s", runtime)
	return hex.EncodeToString(sum.Sum(nil))
}

// nodeRuntime fingerprints the Node and npm executables the app user's PATH
// finds, so upgrading Node reinstalls native modules built for the old one.
func (h Host) nodeRuntime() string {
	return h.executables("node", "npm")
}

func (h Host) composerRuntime() string {
	return services.PHPVersion + "\x00" + h.executables("/usr/bin/php"+services.PHPVersion, "composer")
}

func (h Host) executables(names ...string) string {
	fingerprint := ""
	for _, name := range names {
		candidates := []string{name}
		if !filepath.IsAbs(name) {
			candidates = []string{"/usr/local/bin/" + name, "/usr/bin/" + name, "/bin/" + name}
		}
		found := "missing"
		for _, candidate := range candidates {
			resolved, err := filepath.EvalSymlinks(h.path(candidate))
			if err != nil {
				continue
			}
			if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() {
				found = fmt.Sprintf("%s %d %d", resolved, info.Size(), info.ModTime().UnixNano())
				break
			}
		}
		fingerprint += name + "=" + found + "\x00"
	}
	return fingerprint
}

// projectInputs reads the dependency inputs present in the project directory.
func (h Host) projectInputs(a config.App, names []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, name := range names {
		data, err := readProjectFile(h.path(filepath.Join(a.Directory, name)), maxCommandOutput)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read project %s: %w", name, err)
		}
		files[name] = data
	}
	return files, nil
}

func (h Host) dependencyStampPath(app, tool string) string {
	return filepath.Join(h.Manager.StateDir, "deployments", app, tool+".installed")
}

// installed reports whether the last successful install of tool used key and
// its installed tree is still in place. A failed or interrupted install
// removes the record first, so a half-written tree is never trusted.
func (h Host) installed(a config.App, tool, key string) bool {
	data, err := os.ReadFile(h.path(h.dependencyStampPath(a.Name, tool)))
	if err != nil || string(data) != key {
		return false
	}
	// Written by the package manager once the tree is complete.
	marker := "node_modules/.package-lock.json"
	if tool == "composer" {
		marker = "vendor/composer/installed.json"
	}
	info, err := os.Lstat(h.path(filepath.Join(a.Directory, marker)))
	return err == nil && info.Mode().IsRegular()
}

func (h Host) forgetInstall(a config.App, tool string) error {
	if err := os.Remove(h.path(h.dependencyStampPath(a.Name, tool))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (h Host) rememberInstall(a config.App, tool, key string) error {
	return h.write(h.dependencyStampPath(a.Name, tool), []byte(key), 0600)
}

// rootInstallScripts lists the package's own lifecycle scripts that npm ci
// runs after installing, in npm's order. Skipping npm ci still runs them,
// since their inputs (a Prisma schema, Nuxt sources) can change without the
// lock file changing. npm run starts a script's pre and post hooks itself.
func rootInstallScripts(packageJSON []byte) ([][]string, error) {
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(packageJSON, &manifest); err != nil {
		return nil, fmt.Errorf("package.json is not valid JSON: %w", err)
	}
	var runs [][]string
	for _, event := range []string{"install", "prepublish", "prepare"} {
		if _, ok := manifest.Scripts[event]; ok {
			runs = append(runs, []string{"run", event})
			continue
		}
		for _, hook := range []string{"pre" + event, "post" + event} {
			if _, ok := manifest.Scripts[hook]; ok {
				runs = append(runs, []string{"run", hook})
			}
		}
	}
	return runs, nil
}

// installAppPackages runs npm ci unless the same lock was already installed
// by the same Node and npm, which leaves node_modules exactly as it is.
func (h Host) installAppPackages(a config.App, run func(string, ...string) error) error {
	if h.DryRun {
		h.say("Would skip npm ci if package.json, package-lock.json, .npmrc and Node are unchanged since the last install")
		return run("npm", npmInstallArgs...)
	}
	files, err := h.projectInputs(a, npmInputs)
	if err != nil {
		return err
	}
	key := dependencyKey(h.nodeRuntime(), files)
	if h.installed(a, "npm", key) {
		h.note("  ✓ Node packages unchanged; skipped npm ci")
		scripts, err := rootInstallScripts(files["package.json"])
		if err != nil {
			return err
		}
		for _, args := range scripts {
			if err := run("npm", args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := h.forgetInstall(a, "npm"); err != nil {
		return err
	}
	if err := run("npm", npmInstallArgs...); err != nil {
		return err
	}
	return h.rememberInstall(a, "npm", key)
}
