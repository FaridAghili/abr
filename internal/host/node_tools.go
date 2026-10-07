package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Use Ubuntu's package-download account to unpack npm tools. Root only freezes
// and publishes the completed tree; no npm installer or package scripts get root.
func (h Host) installNodeTools(images bool) (result error) {
	const base = "/opt/abr/node-tools"
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
	var previous struct{ Directory string }
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
	packages := []string{"install", "--global", "--prefix", stage, "--ignore-scripts", "--engine-strict", "--no-audit", "--no-fund", "npm@latest", "npm-check-updates@latest"}
	if images {
		packages = append(packages, "svgo@latest")
	}
	home := filepath.Join(stage, ".home")
	if _, err := h.unprivileged("_apt", home, "/", map[string]string{"NPM_CONFIG_USERCONFIG": "/dev/null", "NPM_CONFIG_GLOBALCONFIG": "/dev/null"}, false, "/usr/bin/npm", packages...); err != nil {
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
	tools := []string{"npm", "npx", "ncu", "npm-check-updates"}
	if images {
		tools = append(tools, "svgo")
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
	data, _ := json.Marshal(struct{ Directory string }{stage})
	if err := h.write(filepath.Join(h.Manager.StateDir, recordPath), data, 0600); err != nil {
		return err
	}
	committed = true
	// Keep unrelated installations. Only the recorded previous tree is retired,
	// and only when none of its public tool links still point into it.
	if previous.Directory != "" {
		for _, tool := range []string{"npm", "npx", "ncu", "npm-check-updates", "svgo"} {
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
