package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const nodeToolsBase = "/opt/abr/node-tools"

type nodeToolsRecord struct{ Directory string }

func (h Host) installNodeTools(images bool) error {
	packages := map[string]string{"npm": "latest", "npm-check-updates": "latest"}
	if images {
		packages["svgo"] = "latest"
	}
	if !h.DryRun {
		directory, err := h.nodeToolsDirectory()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			installed, err := h.globalNodePackages(directory)
			if err != nil {
				return err
			}
			for name, version := range installed {
				if _, selected := packages[name]; !selected {
					packages[name] = version
				}
			}
		}
	}
	if err := h.installNodePackages(nodePackageSpecs(packages)); err != nil {
		return err
	}
	return h.updateNodeTools()
}

// Use Ubuntu's package-download account to unpack npm tools. Root only freezes
// and publishes the completed tree; no npm installer or package scripts get root.
func (h Host) installNodePackages(specs []string) (result error) {
	const base = nodeToolsBase
	if h.DryRun {
		h.say("Would install shared npm/ncu tools as _apt with scripts disabled, then publish root-owned files under %s", base)
		return nil
	}
	entry, exists, err := h.passwd("_apt")
	parts := strings.Split(entry, ":")
	if err != nil {
		return err
	}
	if !exists || len(parts) != 7 || parts[0] != "_apt" || !nonRootIDs(parts[2], parts[3]) {
		return fmt.Errorf("shared tool installation requires Ubuntu's unprivileged _apt account")
	}
	if err := h.trustedAncestor(h.path(base)); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(base), 0755); err != nil {
		return err
	}
	if err := h.trustedDirectory(h.path(base)); err != nil {
		return err
	}
	const recordPath = "node-tools.json"
	var previous nodeToolsRecord
	if data, err := h.read(filepath.Join(h.Manager.StateDir, recordPath)); err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return err
		}
		if filepath.Dir(previous.Directory) != h.path(base) || !strings.HasPrefix(filepath.Base(previous.Directory), "release-") || filepath.Clean(previous.Directory) != previous.Directory {
			return fmt.Errorf("invalid shared node tools ownership record")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	current := h.path(filepath.Join(base, "current"))
	if target, err := os.Readlink(current); err == nil {
		if target != previous.Directory || target == "" {
			return fmt.Errorf("refusing to replace unrecorded node tools link")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(h.path(base), "release-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			result = errors.Join(result, os.RemoveAll(stage))
		}
	}()
	if err := h.command("chown", "--no-dereference", "_apt", "--", stage); err != nil {
		return err
	}
	packages := []string{"install", "--global", "--prefix", stage, "--ignore-scripts", "--engine-strict", "--no-audit", "--no-fund"}
	packages = append(packages, specs...)
	home := filepath.Join(stage, ".home")
	// npm rejects loading the same path as both user and global configuration.
	// Distinct absent files in the private stage avoid inherited host settings.
	npmConfig := map[string]string{
		"NPM_CONFIG_USERCONFIG":   filepath.Join(home, ".npmrc"),
		"NPM_CONFIG_GLOBALCONFIG": filepath.Join(stage, ".npmrc-global"),
	}
	if _, err := h.unprivileged("_apt", home, "/", npmConfig, false, "/usr/bin/npm", packages...); err != nil {
		return err
	}
	// Never follow package symlinks while changing ownership or validating paths.
	if err := h.command("chown", "-hR", "root:root", "--", stage); err != nil {
		return err
	}
	if err := os.RemoveAll(home); err != nil {
		return err
	}
	if err := freezeNodeTools(stage); err != nil {
		return err
	}
	// Give ordinary npm -g and ncu -g the same shared prefix as Abr. npm's
	// built-in configuration supplies defaults without replacing user settings.
	builtin := filepath.Join(stage, "lib/node_modules/npm/npmrc")
	if err := h.trustedFile(builtin); err != nil && !os.IsNotExist(err) {
		return err
	}
	contents, err := os.ReadFile(builtin)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	contents = append(contents, []byte("\n# Shared global tools managed by abr\nprefix="+h.path(filepath.Join(base, "current"))+"\n")...)
	if err := h.write(builtin, contents, 0644); err != nil {
		return err
	}
	tools, err := nodeToolNames(stage)
	if err != nil {
		return err
	}
	for _, required := range []string{"npm", "npx", "ncu", "npm-check-updates"} {
		if !slices.Contains(tools, required) {
			return fmt.Errorf("shared tool %s is missing", required)
		}
	}
	for _, tool := range tools {
		path := filepath.Join(stage, "bin", tool)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(resolved, stage+string(filepath.Separator)) {
			return fmt.Errorf("shared tool %s is missing or escapes its installation", tool)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("shared tool %s is not executable", tool)
		}
	}
	// Execute downloaded code only after the tree is immutable to the installer.
	// Changing ownership does not revoke a writable descriptor opened earlier.
	if _, err := h.unprivileged("_apt", "/nonexistent", "/", nil, false, filepath.Join(stage, "bin/npm"), "--version"); err != nil {
		return err
	}
	if err := h.trustedAncestor(h.path("/usr/local/bin")); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path("/usr/local/bin"), 0755); err != nil {
		return err
	}
	// Back up every symlink before publishing; restore them if publication fails.
	links := map[string]string{current: stage}
	for _, tool := range tools {
		links[h.path("/usr/local/bin/"+tool)] = filepath.Join(stage, "bin", tool)
	}
	paths := make([]string, 0, len(links))
	old := map[string]string{}
	for path := range links {
		target, err := os.Readlink(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("refusing to replace a non-symlink tool %s: %w", path, err)
		}
		old[path] = target
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var changed []string
	defer func() {
		if result != nil && !committed {
			for _, path := range changed {
				if old[path] == "" {
					result = errors.Join(result, os.Remove(path))
				} else {
					result = errors.Join(result, replaceSymlink(path, old[path]))
				}
			}
		}
	}()
	for _, path := range paths {
		if err := replaceSymlink(path, links[path]); err != nil {
			return err
		}
		changed = append(changed, path)
	}
	data, _ := json.Marshal(nodeToolsRecord{stage})
	if err := h.write(filepath.Join(h.Manager.StateDir, recordPath), data, 0600); err != nil {
		return err
	}
	committed = true
	// Keep unrelated installations. Only the recorded previous tree is retired,
	// and only when none of its public tool links still point into it.
	if previous.Directory != "" {
		previousTools, err := nodeToolNames(previous.Directory)
		if err != nil {
			return err
		}
		for _, tool := range previousTools {
			if target, _ := os.Readlink(h.path("/usr/local/bin/" + tool)); strings.HasPrefix(target, previous.Directory+string(filepath.Separator)) {
				return nil
			}
		}
		return os.RemoveAll(previous.Directory)
	}
	return nil
}

func freezeNodeTools(directory string) error {
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(path)
			if filepath.IsAbs(target) || err != nil || !strings.HasPrefix(resolved, directory+string(filepath.Separator)) {
				return fmt.Errorf("unsafe symlink in shared node tools: %s", path)
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular file in shared node tools: %s", path)
		}
		mode := os.FileMode(0644)
		if info.IsDir() || info.Mode().Perm()&0111 != 0 {
			mode = 0755
		}
		return os.Chmod(path, mode)
	})
}

func replaceSymlink(path, target string) error {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".abr-link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	return os.Rename(link, path)
}

func nodeToolNames(directory string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "bin"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(entry.Name()) {
			return nil, fmt.Errorf("invalid shared tool name %q", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func (h Host) nodeToolsDirectory() (string, error) {
	data, err := h.read(filepath.Join(h.Manager.StateDir, "node-tools.json"))
	if err != nil {
		return "", err
	}
	var record nodeToolsRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", err
	}
	if filepath.Dir(record.Directory) != h.path(nodeToolsBase) || !strings.HasPrefix(filepath.Base(record.Directory), "release-") || filepath.Clean(record.Directory) != record.Directory {
		return "", fmt.Errorf("invalid shared node tools ownership record")
	}
	if err := h.trustedDirectory(record.Directory); err != nil {
		return "", err
	}
	target, err := os.Readlink(h.path(filepath.Join(nodeToolsBase, "current")))
	if err != nil || target != record.Directory {
		return "", fmt.Errorf("shared node tools link does not match its ownership record")
	}
	return record.Directory, nil
}

var nodePackageName = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
var nodePackageVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?$`)

func validNodePackage(name, version string) bool {
	return len(name) <= 214 && nodePackageName.MatchString(name) && nodePackageVersion.MatchString(version)
}

func nodePackageSpecs(packages map[string]string) []string {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	slices.Sort(names)
	specs := make([]string, 0, len(names))
	for _, name := range names {
		specs = append(specs, name+"@"+packages[name])
	}
	return specs
}

// Capture JSON without streaming it to logs. All checks are outside projects,
// with isolated npm configuration and an unprivileged, disposable cache.
func (h Host) nodeToolsQuery(prefix, executable string, args ...string) ([]byte, error) {
	entry, exists, err := h.passwd("_apt")
	parts := strings.Split(entry, ":")
	if err != nil {
		return nil, err
	}
	if !exists || len(parts) != 7 || parts[0] != "_apt" || !nonRootIDs(parts[2], parts[3]) {
		return nil, fmt.Errorf("global package checks require Ubuntu's unprivileged _apt account")
	}
	workspace, err := os.MkdirTemp(h.path(nodeToolsBase), ".check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workspace)
	if err := h.command("chown", "--no-dereference", "_apt", "--", workspace); err != nil {
		return nil, err
	}
	command := []string{"--user", "_apt", "--", "env", "-i", "HOME=" + workspace, "USER=_apt", "LOGNAME=_apt", "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin",
		"NPM_CONFIG_USERCONFIG=" + filepath.Join(workspace, ".npmrc"), "NPM_CONFIG_GLOBALCONFIG=" + filepath.Join(workspace, ".npmrc-global")}
	if prefix != "" {
		command = append(command, "NPM_CONFIG_PREFIX="+prefix)
	}
	command = append(command, "/usr/bin/setpriv", "--no-new-privs", "--", executable)
	command = append(command, args...)
	return h.run("Check global npm packages: "+strings.Join(append([]string{executable}, args...), " "), Command{Name: "runuser", Args: command, Dir: "/", Private: true})
}

func (h Host) globalNodePackages(prefix string) (map[string]string, error) {
	out, err := h.nodeToolsQuery(prefix, "/usr/bin/npm", "list", "--global", "--depth=0", "--json")
	if err != nil {
		return nil, err
	}
	var inventory struct {
		Dependencies map[string]struct{ Version string } `json:"dependencies"`
	}
	if err := json.Unmarshal(out, &inventory); err != nil {
		return nil, fmt.Errorf("invalid global npm package inventory: %w", err)
	}
	packages := map[string]string{}
	for name, dependency := range inventory.Dependencies {
		if !validNodePackage(name, dependency.Version) {
			return nil, fmt.Errorf("invalid global npm package %q", name)
		}
		packages[name] = dependency.Version
	}
	return packages, nil
}

func (h Host) updateNodeTools() error {
	if h.DryRun {
		h.say("Would run ncu -g and install all available global npm upgrades, including npm, npm-check-updates and installed SVGO")
		return nil
	}
	directory, err := h.nodeToolsDirectory()
	if err != nil {
		return fmt.Errorf("run abr setup before updating global npm tools: %w", err)
	}
	installed, err := h.globalNodePackages(directory)
	if err != nil {
		return err
	}
	for _, required := range []string{"npm", "npm-check-updates"} {
		if installed[required] == "" {
			return fmt.Errorf("managed global npm package %s is missing", required)
		}
	}
	// Include administrator-installed globals from Node's native prefix too.
	// Abr's published versions take precedence for packages present in both.
	output, err := h.nodeToolsQuery("", "/usr/bin/npm", "prefix", "--global")
	if err != nil {
		return err
	}
	nativePrefix := strings.TrimSpace(string(output))
	if !filepath.IsAbs(nativePrefix) || nativePrefix == "/" || filepath.Clean(nativePrefix) != nativePrefix {
		return fmt.Errorf("invalid native global npm prefix")
	}
	native, err := h.globalNodePackages(nativePrefix)
	if err != nil {
		return err
	}
	upgrades := map[string]string{}
	for index, prefix := range []string{directory, nativePrefix} {
		allowed := map[string]bool{}
		for name := range installed {
			allowed[name] = true
		}
		args := []string{"-g", "--jsonUpgraded", "--install", "never"}
		if index == 1 {
			allowed = map[string]bool{}
			var extra []string
			for name, version := range native {
				if _, managed := installed[name]; !managed {
					extra = append(extra, name)
					allowed[name] = true
					installed[name] = version
				}
			}
			if len(extra) == 0 {
				continue
			}
			slices.Sort(extra)
			args = append(args, "--filter", strings.Join(extra, ","))
		}
		out, err := h.nodeToolsQuery(prefix, filepath.Join(directory, "bin/ncu"), args...)
		if err != nil {
			return err
		}
		var available map[string]string
		if err := json.Unmarshal(out, &available); err != nil {
			return fmt.Errorf("invalid ncu global upgrade output: %w", err)
		}
		if available == nil {
			return fmt.Errorf("invalid ncu global upgrade output: expected a JSON object")
		}
		for name, version := range available {
			if !allowed[name] || !validNodePackage(name, version) {
				return fmt.Errorf("invalid global npm upgrade for %q", name)
			}
			upgrades[name] = version
		}
	}
	if len(upgrades) == 0 {
		h.say("Global npm packages are up to date")
		return nil
	}
	for _, spec := range nodePackageSpecs(upgrades) {
		h.say("Upgrade global npm package %s", spec)
	}
	for name, version := range upgrades {
		installed[name] = version
	}
	return h.installNodePackages(nodePackageSpecs(installed))
}
