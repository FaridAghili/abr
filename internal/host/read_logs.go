package host

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"abr/internal/config"
	"abr/internal/logview"
	"abr/internal/services"
)

type ReadLogsOptions struct {
	All, Follow bool
	Type        string
	Lines       int
}

func (h Host) ReadLogs(name, service string, o ReadLogsOptions) error {
	if o.All == (name != "") {
		return fmt.Errorf("select APP [SERVICE] or --all")
	}
	if o.Type == "" {
		o.Type = "journal"
	}
	if o.Lines == 0 {
		o.Lines = 100
	}
	if o.Lines < 1 || o.Lines > 1000 {
		return fmt.Errorf("--lines must be between 1 and 1000")
	}
	if o.Type != "journal" && o.Type != "application" && o.Type != "deployment" {
		return fmt.Errorf("log type must be journal, application or deployment")
	}
	if service != "" && (o.All || o.Type != "journal") {
		return fmt.Errorf("SERVICE is only used with one app's journal logs")
	}
	if o.Follow && o.Type != "journal" {
		return fmt.Errorf("--follow is only available for journal logs")
	}
	if err := h.guard(); err != nil {
		return err
	}
	cfg, err := config.Load(h.Manager.ConfigPath)
	if err != nil {
		return err
	}
	apps, err := selectedApps(cfg, name)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return fmt.Errorf("no applications selected")
	}
	if o.Type == "journal" {
		return h.readJournalLogs(apps, service, o)
	}
	var failures []error
	// Bound file content for the combined view, even with many large log files.
	byteLimit := max(1, min(16*1024, 96*1024/(len(apps)*3)))
	for _, app := range apps {
		if err := h.readFileLogs(app, o.Type, o.Lines, byteLimit); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", app.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (h Host) readJournalLogs(apps []config.App, service string, o ReadLogsOptions) error {
	var units []string
	for _, app := range apps {
		m, exists, err := h.loadManifest(app)
		if err != nil {
			return err
		}
		if !exists {
			if !o.All {
				return fmt.Errorf("%s has no installed managed services", app.Name)
			}
			h.say("%s: no installed managed services; omitted from journal view", app.Name)
			continue
		}
		if o.All && len(m.Units) == 0 && !m.FPM {
			h.say("%s: no journal units; omitted from journal view", app.Name)
			continue
		}
		selected, err := selectedUnits(app, m, service)
		if err != nil {
			return err
		}
		units = append(units, selected...)
	}
	units = stopUnits(units) // Include scheduler jobs and deduplicate shared FPM.
	if len(units) == 0 {
		return fmt.Errorf("no installed app journal units")
	}
	if slices.Contains(units, "php"+services.PHPVersion+"-fpm") {
		h.say("PHP-FPM is shared: its journal includes all PHP-FPM pools")
	}
	args := []string{"--no-pager", "--output=with-unit", "-n", strconv.Itoa(o.Lines)}
	if o.Follow {
		args = append(args, "--follow")
	}
	for _, unit := range units {
		args = append(args, "--unit", unit)
	}
	if o.Follow || h.DryRun {
		return h.command("journalctl", args...)
	}
	output, err := h.run("Read recent service journals (combined by time; service names identify apps)", Command{Name: "journalctl", Args: args, Private: true})
	if err != nil {
		return err
	}
	data, truncated, err := logview.Tail(bytes.NewReader(output), int64(len(output)), o.Lines, 96*1024)
	if err != nil {
		return err
	}
	if truncated {
		h.say("[Earlier journal content omitted to bound the snapshot]")
	}
	h.say("%s", string(data))
	return nil
}

type logEntry struct {
	root    *os.Root
	name    string
	display string
	updated time.Time
}

func (h Host) readFileLogs(app config.App, kind string, lines, byteLimit int) error {
	base := app.Directory
	relatives := []string{"logs"}
	if kind == "deployment" {
		base = h.Manager.StateDir
		relatives = []string{"deployments/" + app.Name}
	} else if app.Type == "laravel" {
		relatives = append(relatives, "storage/logs")
	}
	if h.DryRun {
		for _, relative := range relatives {
			h.say("Would read %s: last %d lines from up to 3 recent *.log files under %s", app.Name, lines, filepath.Join(base, relative))
		}
		return nil
	}
	if kind == "application" {
		if _, err := os.Lstat(h.path(app.Directory)); err == nil {
			if err := h.project(app); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	var files []logEntry
	for _, relative := range relatives {
		root, err := openLogDirectory(h.path(base), relative)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		defer root.Close()
		entries := 0
		err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			entries++
			if entries > 10000 {
				return fmt.Errorf("log directory exceeds 10000 entries; archive old logs before viewing")
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("log must be a regular file without symlinks: %s", path)
			}
			files = append(files, logEntry{root: root, name: path, display: filepath.Join(base, relative, path), updated: info.ModTime()})
			return nil
		})
		if err != nil {
			return err
		}
	}
	slices.SortFunc(files, func(a, b logEntry) int {
		if order := b.updated.Compare(a.updated); order != 0 {
			return order
		}
		return strings.Compare(a.display, b.display)
	})
	h.say("== %s / %s logs ==", app.Name, kind)
	if len(files) == 0 {
		h.say("No *.log files found")
		return nil
	}
	if len(files) > 3 {
		h.say("Showing the 3 most recently modified log files of %d", len(files))
		files = files[:3]
	}
	for _, file := range files {
		h.say("-- %s --", file.display)
		if err := h.readLogTail(file, lines, byteLimit); err != nil {
			return err
		}
	}
	return nil
}

func (h Host) readLogTail(file logEntry, lines, byteLimit int) error {
	f, err := file.root.OpenFile(file.name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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
	data, truncated, err := logview.Tail(f, info.Size(), lines, byteLimit)
	if err != nil {
		return err
	}
	if truncated {
		h.say("[Earlier contents omitted; showing up to %d lines / %d bytes]", lines, byteLimit)
	}
	if len(data) == 0 {
		h.say("(empty)")
	} else {
		h.say("%s", string(data))
	}
	return nil
}
