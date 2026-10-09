package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"abr/internal/config"
)

type ClearLogsOptions struct {
	All, Yes bool
	Type     string
}

// ClearLogs touches only conventional application log directories and Abr's
// per-app deployment logs. Journald has shared storage and cannot be vacuumed
// per app, so it is deliberately outside this operation.
func (h Host) ClearLogs(names []string, o ClearLogsOptions) error {
	if o.All == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
	}
	if o.Type == "" {
		o.Type = "all"
	}
	if o.Type != "all" && o.Type != "application" && o.Type != "deployment" {
		return fmt.Errorf("log type must be all, application or deployment")
	}
	if !o.Yes && !h.DryRun {
		return fmt.Errorf("use --yes to confirm permanent loss of log contents")
	}
	return h.locked(func() error {
		cfg, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		if o.All {
			for _, a := range cfg.Apps {
				names = append(names, a.Name)
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("no applications selected")
		}
		seen := map[string]bool{}
		var apps []config.App
		// Validate the complete selection before clearing anything.
		for _, name := range names {
			if seen[name] {
				return fmt.Errorf("duplicate log selection %s", name)
			}
			seen[name] = true
			index := slices.IndexFunc(cfg.Apps, func(a config.App) bool { return a.Name == name })
			if index < 0 {
				return fmt.Errorf("unknown application %q", name)
			}
			apps = append(apps, cfg.Apps[index])
		}
		h.say("Selected file logs only; shared system journals are retained")
		var failures []error
		for _, a := range apps {
			if err := h.clearAppLogs(a, o.Type); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", a.Name, err))
			}
		}
		return errors.Join(failures...)
	})
}

func (h Host) clearAppLogs(a config.App, kind string) error {
	type location struct{ base, relative string }
	var locations []location
	if kind != "deployment" {
		if err := h.project(a); err != nil {
			return err
		}
		locations = append(locations, location{a.Directory, "logs"})
		if a.Type == "laravel" {
			locations = append(locations, location{a.Directory, "storage/logs"})
		}
	}
	if kind != "application" {
		locations = append(locations, location{h.Manager.StateDir, "deployments/" + a.Name})
	}
	count := 0
	var failures []error
	for _, loc := range locations {
		if h.DryRun {
			h.say("Would clear %s: *.log files under %s", a.Name, filepath.Join(loc.base, loc.relative))
			continue
		}
		if err := h.checkLogMounts(loc.base, loc.relative); err != nil {
			failures = append(failures, err)
			continue
		}
		n, err := clearLogDirectory(h.path(loc.base), loc.relative)
		count += n
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", filepath.Join(loc.base, loc.relative), err))
		}
	}
	if !h.DryRun {
		if len(failures) > 0 {
			h.say("%s: cleared %d log files; clearing incomplete", a.Name, count)
		} else {
			h.say("%s: cleared %d log files", a.Name, count)
		}
	}
	return errors.Join(failures...)
}

// os.Root prevents symlink escapes, but filesystem mounts can also redirect
// these conventional paths to other data, including bind mounts on the same FS.
func (h Host) checkLogMounts(base, relative string) error {
	if h.root != "" || runtime.GOOS != "linux" {
		return nil
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	path := filepath.Join(base, relative)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := unescape.Replace(fields[4])
		if mount != base && within(base, mount) && (within(path, mount) || within(mount, path)) {
			return fmt.Errorf("unmount filesystems at or inside %s before clearing logs", path)
		}
	}
	return nil
}

func clearLogDirectory(base, relative string) (int, error) {
	root, err := openLogDirectory(base, relative)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	return truncateLogTree(root)
}

func openLogDirectory(base, relative string) (*os.Root, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	// Pin each directory separately. App-owned directories may be renamed or
	// replaced while clearing; a symlink must never redirect root to other data.
	for _, component := range strings.Split(relative, "/") {
		next, err := openLogRoot(root, component)
		if err != nil {
			root.Close()
			return nil, err
		}
		root.Close()
		root = next
	}
	return root, nil
}

func openLogRoot(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("log directory must be a directory without symlinks: %s", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, fmt.Errorf("log directory changed while opening: %s", name)
	}
	parentInfo, err := parent.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if info.Sys().(*syscall.Stat_t).Dev != parentInfo.Sys().(*syscall.Stat_t).Dev {
		root.Close()
		return nil, fmt.Errorf("log directory crosses a filesystem: %s", name)
	}
	return root, nil
}

func truncateLogTree(root *os.Root) (int, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			failures = append(failures, fmt.Errorf("log directory entry must not be a symlink: %s", name))
			continue
		}
		if entry.IsDir() {
			nested, err := openLogRoot(root, name)
			if err == nil {
				var n int
				n, err = truncateLogTree(nested)
				count += n
				nested.Close()
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		if err := truncateLogFile(root, name); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		} else {
			count++
		}
	}
	return count, errors.Join(failures...)
}

func truncateLogFile(root *os.Root, name string) error {
	// Do not use O_TRUNC: validate the opened inode before destroying contents.
	f, err := root.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return fmt.Errorf("log must be a regular file without hard links")
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	return f.Close()
}
