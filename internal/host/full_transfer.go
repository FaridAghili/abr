package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"abr/internal/backup"
	"abr/internal/config"
	"abr/internal/manager"
	"abr/internal/ports"
	"abr/internal/services"
)

// Only these shared files are restored; archive names never become host paths.
func sharedBackupFiles() map[string]string {
	return map[string]string{
		"shared/Caddyfile":   "/etc/caddy/Caddyfile",
		"shared/php-cli.ini": "/etc/php/" + services.PHPVersion + "/cli/conf.d/99-abr.ini",
		"shared/php-fpm.ini": "/etc/php/" + services.PHPVersion + "/fpm/conf.d/99-abr.ini",
		"shared/mysql.cnf":   "/etc/mysql/mysql.conf.d/zz-abr.cnf",
		"shared/redis.conf":  "/etc/redis/abr.conf",
	}
}

type RestoreOptions struct {
	Yes       bool
	AdminUser string
	SSHPort   int
	// Name restores a single-app backup under a new app name.
	Name string
}

func (h Host) backupSettings() (backup.Settings, error) {
	var o SetupOptions
	data, err := h.read(filepath.Join(h.Manager.StateDir, "setup.json"))
	if err == nil {
		if err := json.Unmarshal(data, &o); err != nil {
			return backup.Settings{}, err
		}
	} else if !os.IsNotExist(err) {
		return backup.Settings{}, err
	}
	hostname, err := h.serverHostname()
	if err != nil {
		return backup.Settings{}, err
	}
	if o.RoadRunnerVersion == "" {
		o.RoadRunnerVersion = DefaultRoadRunnerVersion
	}
	// Redis is captured whenever it is managed, even if later setup skipped it.
	_, err = os.Lstat(h.path("/etc/redis/abr.conf"))
	if err != nil && !os.IsNotExist(err) {
		return backup.Settings{}, err
	}
	return backup.Settings{Hostname: hostname, RoadRunnerVersion: o.RoadRunnerVersion, NoFirewall: o.NoFirewall, NoImages: o.NoImages, NoRedis: os.IsNotExist(err)}, nil
}

func (h Host) backupRepositories(c config.Config) ([]backup.App, error) {
	var records []backup.App
	for _, a := range c.Apps {
		if err := h.project(a); err != nil {
			return nil, err
		}
		// Metadata is read as the app user, not root through a project-controlled Git config.
		repo, err := h.asUser(a, nil, true, "git", "remote", "get-url", "origin")
		if err != nil {
			return nil, err
		}
		branch, err := h.asUser(a, nil, true, "git", "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("%s: backup requires a named Git branch: %w", a.Name, err)
		}
		record := backup.App{Name: a.Name, Repository: strings.TrimSpace(string(repo)), Branch: strings.TrimSpace(string(branch))}
		commit, err := h.asUser(a, nil, true, "git", "rev-parse", "--verify", "HEAD")
		if err != nil {
			return nil, err
		}
		record.Commit = strings.TrimSpace(string(commit))
		if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(record.Commit) {
			return nil, fmt.Errorf("%s: invalid backup commit", a.Name)
		}
		if !githubRepository.MatchString(record.Repository) {
			return nil, fmt.Errorf("%s: origin must be a GitHub SSH repository", a.Name)
		}
		if record.Branch == "" || strings.HasPrefix(record.Branch, "-") || strings.ContainsAny(record.Branch, "\x00\r\n") {
			return nil, fmt.Errorf("%s: invalid Git branch", a.Name)
		}
		if _, err := h.asUser(a, nil, true, "git", "check-ref-format", "--branch", record.Branch); err != nil {
			return nil, err
		}
		m, _, err := h.loadManifest(a)
		if err != nil {
			return nil, err
		}
		record.Enabled = m.Enabled
		records = append(records, record)
	}
	return records, nil
}

func copyBackupFile(source, destination string, optional bool) error {
	info, err := os.Lstat(source)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup source must be a regular file: %s", source)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(source))
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(filepath.Dir(source)) {
		return fmt.Errorf("backup file parents must not be symlinks")
	}
	root, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(source), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup source changed type")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, f)
	return errors.Join(copyErr, out.Close())
}

// stageLiveTree opens parent/rel for backup.Write to stream in place of an
// empty staged directory. The tree is resolved beneath parent, which its owner
// cannot replace, so a swapped link cannot redirect reads outside parent. A
// missing tree is skipped; rel itself must be a directory, not a symlink.
func stageLiveTree(live map[string]*os.Root, stage, name, parent, rel string) error {
	resolved, err := filepath.EvalSymlinks(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(parent) {
		return fmt.Errorf("backup directory parents must not be symlinks")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("backup source must be a directory without symlinks: %s", filepath.Join(parent, rel))
	}
	tree, err := root.OpenRoot(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stage, filepath.FromSlash(name)), 0700); err != nil {
		tree.Close()
		return err
	}
	live[name] = tree
	return nil
}

func closeLiveTrees(live map[string]*os.Root) {
	for _, tree := range live {
		tree.Close()
	}
}

// FullBackup produces one complete, root-only archive. It publishes only after
// every capture and checksum has succeeded. Services remain running.
func (h Host) FullBackup(output string) error {
	if !filepath.IsAbs(output) || !strings.HasSuffix(output, ".tar.gz") || strings.ContainsAny(output, "\x00\r\n") {
		return fmt.Errorf("use an absolute --output FILE.tar.gz")
	}
	return h.locked(func() error {
		if h.DryRun {
			c, err := config.Load(h.Manager.ConfigPath)
			if err != nil {
				return err
			}
			h.say("Would capture %d managed apps online: MySQL, Redis, .env, full Laravel storage/app and saved source, settings, templates and credentials into %s; services remain running; no files changed", len(c.Apps), output)
			return nil
		}
		return h.Manager.WithSnapshot(func(c config.Config, r ports.Registry) (result error) {
			if len(c.Apps) == 0 {
				return fmt.Errorf("no registered apps to back up")
			}
			if err := manager.CheckReservations(c, r); err != nil {
				return err
			}
			if err := h.trustedAncestor(filepath.Dir(output)); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
				return err
			}
			if err := h.trustedDirectory(filepath.Dir(output)); err != nil {
				return err
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				return fmt.Errorf("backup output must not exist")
			}
			// Keep the archive outside every captured tree, even with custom paths.
			for _, source := range append([]string{h.Manager.StateDir, h.TemplatesDir, h.AppsDir, h.path("/var/lib/redis"), h.path("/var/lib/caddy/.local/share/caddy")}, appDirectories(c)...) {
				if within(source, output) {
					return fmt.Errorf("backup output must be outside managed data directories")
				}
			}
			records, err := h.backupRepositories(c)
			if err != nil {
				return err
			}
			settings, err := h.backupSettings()
			if err != nil {
				return err
			}
			stage, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".full-backup-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(stage)
			live := map[string]*os.Root{}
			defer closeLiveTrees(live)
			dbRecords := map[string]credentials{}
			for _, a := range c.Apps {
				if a.Type == "laravel" {
					if _, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env"))); err != nil {
						return fmt.Errorf("%s: .env required: %w", a.Name, err)
					}
				}
				_, err := os.Lstat(h.path(h.credentialsPath(a.Name)))
				if os.IsNotExist(err) && a.Type == "laravel" {
					return fmt.Errorf("%s: full backup requires recorded managed MySQL credentials", a.Name)
				}
				if !os.IsNotExist(err) || a.Database.Enabled {
					dbApp := a
					dbApp.Database.Enabled = true
					creds, err := h.transferCredentials(dbApp)
					if err != nil {
						return err
					}
					dbRecords[a.Name] = creds
				}
			}
			// Capture only recorded shared credentials and editable templates.
			for _, name := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json", "mysql-admin.json"} {
				if err := copyBackupFile(h.path(filepath.Join(h.Manager.StateDir, name)), filepath.Join(stage, "state", name), name != "git/id_ed25519" && name != "git/known_hosts"); err != nil {
					return err
				}
			}
			if err := h.copyBackupTemplates(filepath.Join(stage, "templates")); err != nil {
				return err
			}
			for name, creds := range dbRecords {
				if err := os.MkdirAll(filepath.Join(stage, "sql"), 0700); err != nil {
					return err
				}
				if err := h.dumpDatabase(creds, filepath.Join(stage, "sql", name+".sql")); err != nil {
					return err
				}
				data, err := json.Marshal(creds)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(stage, "state/databases"), 0700); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(stage, "state/databases", name+".json"), data, 0600); err != nil {
					return err
				}
			}
			for _, a := range c.Apps {
				record := records[slices.IndexFunc(records, func(record backup.App) bool { return record.Name == a.Name })]
				if err := h.captureBackupCode(a, record, stage); err != nil {
					return err
				}
				if err := copyBackupFile(h.path(filepath.Join(a.Directory, ".env")), filepath.Join(stage, "apps", a.Name, ".env"), a.Type == "nuxt"); err != nil {
					return err
				}
				if a.Type == "laravel" {
					if err := stageLiveTree(live, stage, "apps/"+a.Name+"/storage/app", h.path(a.Directory), "storage/app"); err != nil {
						return err
					}
				}
			}
			for name, source := range sharedBackupFiles() {
				if err := copyBackupFile(h.path(source), filepath.Join(stage, name), true); err != nil {
					return err
				}
			}
			if !settings.NoRedis {
				if err := os.MkdirAll(filepath.Join(stage, "redis"), 0700); err != nil {
					return err
				}
				if _, err := h.run("Capture online Redis RDB snapshot", Command{Name: "redis-cli", Args: []string{"--rdb", filepath.Join(stage, "redis/dump.rdb")}, Private: true}); err != nil {
					return err
				}
			}
			if err := stageLiveTree(live, stage, "caddy", h.path("/var/lib/caddy"), ".local/share/caddy"); err != nil {
				return err
			}
			f, err := os.CreateTemp(filepath.Dir(output), ".abr-backup-")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			defer f.Close()
			m := backup.Manifest{Version: 1, CreatedAt: time.Now().UTC(), Config: c, Ports: r, Apps: records, Settings: settings}
			if err := backup.Write(f, stage, &m, live); err != nil {
				return err
			}
			if err := validateFullBackup(m, stage); err != nil {
				return err
			}
			if err := f.Sync(); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Link(f.Name(), output); err != nil {
				return err
			}
			dir, err := os.Open(filepath.Dir(output))
			if err != nil {
				return err
			}
			defer dir.Close()
			if err := dir.Sync(); err != nil {
				return err
			}
			h.say("Full backup complete: %s (mode 0600; contains secrets). Copy it off this server.", output)
			return nil
		})
	})
}

func appDirectories(c config.Config) []string {
	var dirs []string
	for _, a := range c.Apps {
		dirs = append(dirs, a.Directory)
	}
	return dirs
}

// FullRestore validates the complete archive before provisioning, and refuses to
// replace existing projects or adopt existing users/databases.
func (h Host) FullRestore(input string, o RestoreOptions) error {
	return h.RestoreArchives([]string{input}, o)
}

// RestoreArchives provisions a fresh server from its saved settings, then
// restores every app. On a server that is already set up, app archives are
// added as new apps and this server's settings and shared data are kept. A
// single directory selects the newest app archive of each app in it.
func (h Host) RestoreArchives(inputs []string, o RestoreOptions) error {
	if !o.Yes && !h.DryRun {
		return fmt.Errorf("restore imports SQL and, from full backups, Redis data; pass --yes to confirm")
	}
	if len(inputs) == 0 {
		return fmt.Errorf("select at least one backup archive")
	}
	if len(inputs) == 1 && filepath.IsAbs(inputs[0]) && !strings.HasSuffix(inputs[0], ".tar.gz") {
		files, err := backup.LatestArchives(inputs[0])
		if err != nil {
			return err
		}
		names := make([]string, len(files))
		for i, file := range files {
			names[i] = filepath.Base(file)
		}
		h.say("Selected the newest backup of each app: %s", strings.Join(names, ", "))
		inputs = files
	}
	for _, input := range inputs {
		if !filepath.IsAbs(input) || !strings.HasSuffix(input, ".tar.gz") || strings.ContainsAny(input, "\x00\r\n") {
			return fmt.Errorf("use absolute backup FILE.tar.gz paths or one directory of app backups")
		}
	}
	if o.SSHPort < 0 || o.SSHPort > 65535 {
		return fmt.Errorf("invalid SSH port")
	}
	if o.Name != "" {
		if !config.ValidName(o.Name) {
			return fmt.Errorf("--name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens (max 63 characters)")
		}
		if len(inputs) != 1 {
			return fmt.Errorf("--name restores one app backup; select its archive instead of %d", len(inputs))
		}
	}
	if err := h.guard(); err != nil {
		return err
	}
	// Archives can hold gigabytes of uploads and /tmp may be RAM-backed, so
	// extract on disk. A preview extracts only here, never to host paths.
	parent, err := os.MkdirTemp(h.path("/var/tmp"), "abr-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(parent)
	if _, err := os.Lstat(h.path(filepath.Join(h.Manager.StateDir, "setup.json"))); err == nil {
		return h.restoreIntoServer(inputs, o, parent)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage := filepath.Join(parent, "restore")
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	var m backup.Manifest
	for _, input := range inputs {
		if err := h.extractRestoreArchive(input, stage, &m); err != nil {
			return err
		}
	}
	if err := validateFullBackup(m, stage); err != nil {
		return err
	}
	if err := h.renameRestore(&m, stage, o.Name); err != nil {
		return err
	}
	for i := range m.Config.Apps {
		m.Config.Apps[i].Directory = filepath.Join(h.AppsDir, m.Config.Apps[i].Name)
	}
	if err := m.Config.Validate(); err != nil {
		return err
	}
	if h.DryRun {
		if err := h.freshRestoreTarget(m, false); err != nil {
			return err
		}
		h.say("Restore preview: %d apps; hostname %s; MySQL dumps and credentials; Redis data: %t; saved source commits", len(m.Apps), m.Settings.Hostname, !m.Settings.NoRedis && m.Scope != "app")
		for _, record := range m.Apps {
			h.say("Would restore saved code %s on branch %s into %s from its bundled repository, restore .env/storage/SQL, install dependencies and build with saved settings, migrate and optimize", record.Commit, record.Branch, filepath.Join(h.AppsDir, record.Name))
		}
		h.say("Would provision Ubuntu shared runtimes, recreate users/accounts, regenerate services and ports, restore included shared data, then enable previously enabled apps; no host changes")
		return nil
	}
	return h.locked(func() (result error) {
		h.lockHeld = true
		if err := h.freshRestoreTarget(m, true); err != nil {
			return err
		}
		// Install saved templates and admin password before setup consumes them.
		if err := backup.CopyTree(filepath.Join(stage, "templates"), h.path(h.TemplatesDir)); err != nil {
			return err
		}
		if data, err := os.ReadFile(filepath.Join(stage, "state/mysql-admin.json")); err == nil {
			var c mysqlAdminCredentials
			if err := json.Unmarshal(data, &c); err != nil {
				return err
			}
			c.Ready = false
			data, _ = json.Marshal(c)
			if err := h.write(h.mysqlAdminPath(), data, 0600); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		settings := m.Settings
		setup := SetupOptions{Hostname: settings.Hostname, RoadRunnerVersion: settings.RoadRunnerVersion, AdminUser: o.AdminUser, SSHPort: o.SSHPort, NoFirewall: settings.NoFirewall, NoRedis: settings.NoRedis, NoImages: settings.NoImages}
		if err := h.Setup(setup); err != nil {
			return fmt.Errorf("restore setup: %w", err)
		}
		if err := h.restoreDatabasePreflight(m); err != nil {
			return err
		}
		if err := h.Manager.RestoreSnapshot(m.Config, m.Ports); err != nil {
			return err
		}
		return h.restoreApplications(m, stage)
	})
}

func (h Host) extractRestoreArchive(input, destination string, combined *backup.Manifest) error {
	if combined.Files == nil {
		m, err := h.extractArchive(input, destination)
		if err != nil {
			return err
		}
		*combined = m
		return nil
	}
	// A sibling extraction on the same filesystem lets Merge move, not copy.
	stage, err := os.MkdirTemp(filepath.Dir(destination), "archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	m, err := h.extractArchive(input, stage)
	if err != nil {
		return err
	}
	return backup.Merge(combined, m, stage, destination)
}

// extractArchive extracts and checks one archive in an empty private directory.
func (h Host) extractArchive(input, destination string) (backup.Manifest, error) {
	f, err := os.OpenFile(input, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return backup.Manifest{}, err
	}
	if !info.Mode().IsRegular() {
		return backup.Manifest{}, fmt.Errorf("backup must be a regular file: %s", input)
	}
	m, err := backup.Extract(f, destination)
	if err != nil {
		return m, fmt.Errorf("invalid backup %s: %w", filepath.Base(input), err)
	}
	if err := validateFullBackup(m, destination); err != nil {
		return m, fmt.Errorf("invalid backup %s: %w", filepath.Base(input), err)
	}
	return m, nil
}

// restoreIntoServer adds app archives to this set-up server as new apps. Its
// settings, templates, shared runtime configuration and Redis data are kept.
// Saved ports are reused when free in this server's range; others are new.
func (h Host) restoreIntoServer(inputs []string, o RestoreOptions, parent string) error {
	if o.AdminUser != "" || o.SSHPort != 0 {
		return fmt.Errorf("--admin-user and --ssh-port apply only to a fresh server; this server is already set up")
	}
	type archive struct {
		m     backup.Manifest
		stage string
	}
	var archives []archive
	var apps []config.App
	var records []backup.App
	for i, input := range inputs {
		stage := filepath.Join(parent, strconv.Itoa(i))
		if err := os.Mkdir(stage, 0700); err != nil {
			return err
		}
		m, err := h.extractArchive(input, stage)
		if err != nil {
			return err
		}
		if m.Scope != "app" {
			return fmt.Errorf("%s is a full-server backup; restore it on a fresh server, or restore app backups here", filepath.Base(input))
		}
		if err := h.renameRestore(&m, stage, o.Name); err != nil {
			return err
		}
		for i := range m.Config.Apps {
			m.Config.Apps[i].Directory = filepath.Join(h.AppsDir, m.Config.Apps[i].Name)
		}
		archives = append(archives, archive{m, stage})
		apps = append(apps, m.Config.Apps...)
		records = append(records, m.Apps...)
	}
	probe := h.Manager.Probe
	if probe == nil {
		probe = ports.CheckAvailable
	}
	// plan checks every app against this server and assigns ports in memory.
	plan := func(hostChecks bool) (config.Config, ports.Registry, error) {
		c, err := config.Load(h.Manager.ConfigPath)
		if errors.Is(err, os.ErrNotExist) {
			c, err = config.Default(), nil
		}
		if err != nil {
			return c, ports.Registry{}, err
		}
		r, err := ports.Load(h.Manager.RegistryPath())
		if err != nil {
			return c, r, err
		}
		for i, a := range apps {
			if slices.ContainsFunc(c.Apps, func(existing config.App) bool { return existing.Name == a.Name }) {
				return c, r, fmt.Errorf("%s is already registered on this server; existing app preserved", a.Name)
			}
			if slices.ContainsFunc(apps[:i], func(other config.App) bool { return other.Name == a.Name }) {
				return c, r, fmt.Errorf("%s is selected in more than one backup; choose one", a.Name)
			}
		}
		c.Apps = append(c.Apps, apps...)
		if err := c.Validate(); err != nil {
			return c, r, err
		}
		for _, x := range archives {
			for _, a := range x.m.Config.Apps {
				if err := h.freshAppTarget(a, hostChecks); err != nil {
					return c, r, err
				}
				if err := r.Ensure(a, c.Ports, restoreImports(a, x.m.Ports, r, c.Ports, probe), probe); err != nil {
					return c, r, err
				}
				if _, err := services.Render(a, r, h.TemplatesDir, h.Manager.StateDir); err != nil {
					return c, r, err
				}
			}
		}
		return c, r, nil
	}
	if h.DryRun {
		_, r, err := plan(false)
		if err != nil {
			return err
		}
		h.say("Restore preview: %d apps into this server; its settings, templates, shared runtime configuration, Redis data and other apps are kept", len(apps))
		for _, a := range apps {
			record := restoreRecord(records, a.Name)
			var reserved []string
			for _, assignment := range r.Assignments {
				if assignment.App == a.Name {
					reserved = append(reserved, fmt.Sprintf("%s %d", assignment.Purpose, assignment.Port))
				}
			}
			h.say("Would restore %s: saved code %s on branch %s into %s from its bundled repository; ports: %s; restore .env/storage/SQL, build, migrate and optimize; start afterwards: %t", a.Name, record.Commit, record.Branch, a.Directory, strings.Join(reserved, ", "), record.Enabled)
		}
		h.say("No host changes")
		return nil
	}
	return h.locked(func() (result error) {
		h.lockHeld = true
		c, _, err := plan(true)
		if err != nil {
			return err
		}
		for _, x := range archives {
			if err := h.restoreDatabasePreflight(x.m); err != nil {
				return err
			}
		}
		var restored []config.App
		defer h.stopFailedRestore(&restored, &result, "Remove each restored app with abr remove NAME --purge --yes, then retry; other apps were not changed.")
		for _, x := range archives {
			if err := h.restoreSharedCredentials(x.stage, true); err != nil {
				return err
			}
		}
		var r ports.Registry
		for _, x := range archives {
			for _, a := range x.m.Config.Apps {
				current, err := ports.Load(h.Manager.RegistryPath())
				if err != nil {
					return err
				}
				if r, err = h.Manager.Register(a, restoreImports(a, x.m.Ports, current, c.Ports, probe)); err != nil {
					return err
				}
				restored = append(restored, a)
				if err := h.restoreApplication(a, restoreRecord(records, a.Name), x.stage, x.m); err != nil {
					return err
				}
			}
		}
		if err := h.startRestoredApps(restored, records, r); err != nil {
			return err
		}
		if slices.ContainsFunc(restored, func(a config.App) bool { return a.Backup.Schedule != "" }) {
			if err := h.ensureBackupTimer(); err != nil {
				return err
			}
		}
		h.say("App restore complete: %d apps rebuilt from saved source commits. Point their DNS at this server; keep the old server's workers stopped.", len(restored))
		return nil
	})
}

// renameRestore restores an extracted single-app archive under name. Every
// resource abr derives from the app name follows it: project directory,
// runtime user, state records, MySQL database and accounts, services and port
// reservations. Domains, the saved password and other app settings are kept.
func (h Host) renameRestore(m *backup.Manifest, stage, name string) error {
	if name == "" {
		return nil
	}
	if len(m.Config.Apps) != 1 {
		return fmt.Errorf("--name restores one app backup; this archive holds %d apps", len(m.Config.Apps))
	}
	old := m.Config.Apps[0]
	if name == old.Name {
		return nil
	}
	if err := backup.RenameApp(m, stage, name); err != nil {
		return err
	}
	a := &m.Config.Apps[0]
	a.User, a.Directory = RuntimeUser(name), filepath.Join(h.AppsDir, name)
	env := "apps/" + name + "/.env"
	credentialName := "state/databases/" + name + ".json"
	database := "no managed database"
	if _, ok := m.Files[credentialName]; ok {
		data, err := readProjectFile(filepath.Join(stage, credentialName), 1<<20)
		if err != nil {
			return err
		}
		var c credentials
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.App != old.Name || c.Database != databaseName(old.Name) || c.User != databaseUser(old.Name) {
			return fmt.Errorf("invalid backup database ownership record")
		}
		// The saved password moves to the new accounts with the database.
		c.App, c.Database, c.User = name, databaseName(name), databaseUser(name)
		data, err = json.Marshal(c)
		if err != nil {
			return err
		}
		if err := backup.ReplaceFile(m, stage, credentialName, data); err != nil {
			return err
		}
		if _, ok := m.Files[env]; ok {
			data, err := readProjectFile(filepath.Join(stage, filepath.FromSlash(env)), 1<<20)
			if err != nil {
				return err
			}
			if err := backup.ReplaceFile(m, stage, env, setEnvValues(data, [][2]string{{"DB_DATABASE", c.Database}, {"DB_USERNAME", c.User}})); err != nil {
				return err
			}
		}
		database = "MySQL database " + c.Database + " and account " + c.User
	}
	h.say("Restoring %s as %s: project %s, user %s, %s; services, ports and records use the new name; domains are unchanged", old.Name, name, a.Directory, a.User, database)
	if _, ok := m.Files[env]; ok {
		data, err := readProjectFile(filepath.Join(stage, filepath.FromSlash(env)), 1<<20)
		if err != nil {
			return err
		}
		for _, key := range renamedEnvReview(data, []string{old.Directory, filepath.Join(h.AppsDir, old.Name)}) {
			h.say("Review %s in %s's .env after restore: %s", key, name, renamedEnvReason(key))
		}
	}
	return validateFullBackup(*m, stage)
}

// renamedEnvReview lists .env keys a rename cannot safely rewrite: values
// naming the old project directory, and connection URLs that override DB_*.
func renamedEnvReview(data []byte, directories []string) []string {
	var patterns []*regexp.Regexp
	for _, directory := range directories {
		patterns = append(patterns, regexp.MustCompile(regexp.QuoteMeta(filepath.Clean(directory))+`(?:[^a-z0-9-]|$)`))
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)
		if !ok || strings.HasPrefix(key, "#") || value == "" || slices.Contains(keys, key) {
			continue
		}
		if key == "DB_URL" || key == "DATABASE_URL" || slices.ContainsFunc(patterns, func(p *regexp.Regexp) bool { return p.MatchString(value) }) {
			keys = append(keys, key)
		}
	}
	return keys
}

func renamedEnvReason(key string) string {
	if key == "DB_URL" || key == "DATABASE_URL" {
		return "it overrides the renamed DB_* values"
	}
	return "it names the old project directory"
}

// restoreImports keeps an app's saved ports that are free and inside this
// server's range; every other endpoint is allocated a new port.
func restoreImports(a config.App, saved, current ports.Registry, pool config.Range, probe ports.Probe) map[string]int {
	used := map[int]bool{}
	for _, assignment := range current.Assignments {
		used[assignment.Port] = true
	}
	imports := map[string]int{}
	for _, purpose := range a.Endpoints() {
		port, ok := saved.Lookup(a.Name, purpose)
		if ok && port >= pool.First && port <= pool.Last && !used[port] && probe(port) == nil {
			imports[purpose] = port
			used[port] = true
		}
	}
	return imports
}

func restoreRecord(records []backup.App, name string) backup.App {
	return records[slices.IndexFunc(records, func(record backup.App) bool { return record.Name == name })]
}

// Shared runtimes and the empty-target snapshot are installed before this phase.
func (h Host) restoreApplications(m backup.Manifest, stage string) (result error) {
	// All new apps stay stopped until every build/import succeeds.
	apps := m.Config.Apps
	defer h.stopFailedRestore(&apps, &result, "Partial data is retained for inspection.")
	if err := h.restoreSharedCredentials(stage, false); err != nil {
		return err
	}
	for _, a := range m.Config.Apps {
		if err := h.restoreApplication(a, restoreRecord(m.Apps, a.Name), stage, m); err != nil {
			return err
		}
	}
	if err := h.restoreSharedData(stage, m); err != nil {
		return err
	}
	if err := h.startRestoredApps(m.Config.Apps, m.Apps, m.Ports); err != nil {
		return err
	}
	h.say("Full restore complete: %d apps rebuilt from saved source commits. Check app health and point DNS at this server; keep the old server's workers stopped.", len(m.Apps))
	return nil
}

// stopFailedRestore stops every restored app after a failure; it runs deferred.
func (h Host) stopFailedRestore(apps *[]config.App, result *error, retry string) {
	if *result == nil || len(*apps) == 0 {
		return
	}
	var stopErrors []error
	for _, a := range *apps {
		if err := h.disable(a); err != nil {
			stopErrors = append(stopErrors, err)
		}
	}
	*result = errors.Join(*result, errors.Join(stopErrors...))
	if len(stopErrors) == 0 {
		h.say("Restore failed; restored app services are stopped. %s", retry)
	} else {
		h.say("Restore failed; some apps could not be stopped. Check service status. %s", retry)
	}
}

// restoreSharedCredentials installs the saved Git identity and Composer
// credentials. With keep, this server's own are kept and only missing ones
// are installed.
func (h Host) restoreSharedCredentials(stage string, keep bool) error {
	for _, group := range []struct {
		names []string
		kept  string
	}{
		{[]string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts"}, "Kept this server's Git deploy key; give it read access to the restored repositories before their next deployment"},
		{[]string{"composer/auth.json"}, "Kept this server's Composer credentials; add any private repository credentials the restored apps need"},
	} {
		saved, err := os.ReadFile(filepath.Join(stage, "state", group.names[0]))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if keep {
			current, err := h.read(filepath.Join(h.Manager.StateDir, group.names[0]))
			if err == nil {
				if !bytes.Equal(current, saved) {
					h.say("%s", group.kept)
				}
				continue
			}
			if !os.IsNotExist(err) {
				return err
			}
		}
		for _, name := range group.names {
			data, err := os.ReadFile(filepath.Join(stage, "state", name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := h.write(filepath.Join(h.Manager.StateDir, name), data, 0600); err != nil {
				return err
			}
		}
		if group.names[0] == "composer/auth.json" {
			// The backup records credentials; recreate Composer's generated home
			// guard before deployments grant apps read access to that home.
			if err := h.writeComposerGuard(); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreApplication recreates one app's user, saved source, files and database.
func (h Host) restoreApplication(a config.App, record backup.App, stage string, m backup.Manifest) error {
	if err := h.cloneRepository(record.Repository, a.Directory, filepath.Join(stage, "apps", a.Name, "repository.bundle"), record.Branch); err != nil {
		return fmt.Errorf("clone %s: %w", a.Name, err)
	}
	if err := h.ensureUser(a); err != nil {
		return err
	}
	if err := h.permissions(a); err != nil {
		return err
	}
	if _, err := h.asUser(a, nil, true, "git", "checkout", record.Branch); err != nil {
		return fmt.Errorf("checkout %s: %w", a.Name, err)
	}
	commit, err := h.asUser(a, nil, true, "git", "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(string(commit)) != record.Commit {
		return fmt.Errorf("%s: bundled source commit differs from manifest", a.Name)
	}
	if _, err := h.asUser(a, nil, true, "git", "remote", "set-url", "origin", record.Repository); err != nil {
		return err
	}
	if err := h.restoreWorkingSource(stage, a, m); err != nil {
		return err
	}
	if err := restoreAppFiles(stage, a, h); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(stage, "state/databases", a.Name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	c.Ready, c.TCPManaged, c.TCPReady = false, false, false
	data, _ = json.Marshal(c)
	if err := h.write(h.credentialsPath(a.Name), data, 0600); err != nil {
		return err
	}
	dbApp := a
	dbApp.Database.Enabled = true
	if err := h.database(dbApp, false); err != nil {
		return err
	}
	return h.importDatabaseFile(dbApp, filepath.Join(stage, "sql", a.Name+".sql"))
}

// startRestoredApps builds every restored app with its services stopped, then
// starts only the apps that were enabled when they were backed up.
func (h Host) startRestoredApps(apps []config.App, records []backup.App, r ports.Registry) error {
	for _, a := range apps {
		if err := h.deploy(a, r, DeployOptions{NoPull: true, deferEnable: true}); err != nil {
			return fmt.Errorf("restore deployment %s: %w", a.Name, err)
		}
	}
	for _, a := range apps {
		if restoreRecord(records, a.Name).Enabled {
			if err := h.enable(a, r); err != nil {
				return err
			}
			if err := h.deploymentHealth(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Host) freshRestoreTarget(m backup.Manifest, hostChecks bool) error {
	c, err := config.Load(h.Manager.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(c.Apps) != 0 {
		return fmt.Errorf("full restore requires a fresh server with no registered apps")
	}
	r, err := ports.Load(h.Manager.RegistryPath())
	if err != nil {
		return err
	}
	if len(r.Assignments) != 0 {
		return fmt.Errorf("full restore requires an empty port registry")
	}
	for _, name := range []string{"apps", "users", "databases", "credentials", "env", "git", "composer", "mysql-admin.json", "setup.json"} {
		if _, err := os.Lstat(h.path(filepath.Join(h.Manager.StateDir, name))); !os.IsNotExist(err) {
			return fmt.Errorf("full restore requires fresh abr state; found %s", name)
		}
	}
	for _, a := range m.Config.Apps {
		if err := h.freshAppTarget(a, hostChecks); err != nil {
			return err
		}
	}
	for _, assignment := range m.Ports.Assignments {
		probe := h.Manager.Probe
		if probe == nil {
			probe = ports.CheckAvailable
		}
		if err := probe(assignment.Port); err != nil {
			return fmt.Errorf("restore port %d: %w", assignment.Port, err)
		}
	}
	return nil
}

// freshAppTarget refuses to replace or adopt an app's project, account or records.
func (h Host) freshAppTarget(a config.App, hostChecks bool) error {
	for _, name := range []string{a.Directory, "/var/lib/abr-users/" + a.User, h.manifestPath(a.Name), h.credentialsPath(a.Name), h.userPath(a)} {
		if _, err := os.Lstat(h.path(name)); err == nil {
			return fmt.Errorf("restore target already exists: %s; existing data preserved", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if hostChecks {
		if _, exists, err := h.passwd(a.User); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("restore user %s already exists", a.User)
		}
	}
	return nil
}

func restoreAppFiles(stage string, a config.App, h Host) error {
	data, err := os.ReadFile(filepath.Join(stage, "apps", a.Name, ".env"))
	if err == nil {
		// Write through the dedicated user using the existing atomic environment writer.
		if err := h.writeEnvironment(a, data); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if a.Type == "laravel" {
		// Copy while the fresh checkout is still solely under abr's control. Root
		// operations are bounded by os.Root and never follow links outside the project.
		source := filepath.Join(stage, "apps", a.Name, "storage/app")
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			return h.permissions(a)
		} else if err != nil {
			return err
		}
		projectRoot, err := os.OpenRoot(h.path(a.Directory))
		if err != nil {
			return err
		}
		targetRoot, err := openRestoreDirectory(projectRoot, "storage/app")
		if err == nil {
			err = copyIntoRoot(source, targetRoot)
			targetRoot.Close()
		}
		projectRoot.Close()
		if err != nil {
			return err
		}
	}
	return h.permissions(a)
}

func copyIntoRoot(source string, target *os.Root) error {
	// CopyTree's destination is a private staging tree; bounded installation below
	// ensures even symlinks in the freshly fetched repository cannot escape it.
	return filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, name)
		if err != nil || rel == "." {
			return err
		}
		if entry.IsDir() {
			directory, err := openRestoreDirectory(target, rel)
			if err == nil {
				err = directory.Close()
			}
			return err
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		parent, err := openRestoreDirectory(target, filepath.Dir(rel))
		if err != nil {
			return err
		}
		defer parent.Close()
		out, err := parent.OpenFile(filepath.Base(rel), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return err
		}
		info, err := out.Stat()
		if err != nil || !info.Mode().IsRegular() {
			out.Close()
			return fmt.Errorf("invalid restore destination")
		}
		_, err = io.Copy(out, f)
		return errors.Join(err, out.Close())
	})
}

func (h Host) restoreSharedData(stage string, m backup.Manifest) error {
	if err := h.command("systemctl", "stop", "caddy", "php"+services.PHPVersion+"-fpm"); err != nil {
		return err
	}
	for name, target := range sharedBackupFiles() {
		data, err := os.ReadFile(filepath.Join(stage, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := h.write(target, data, 0644); err != nil {
			return err
		}
		if name == "shared/redis.conf" {
			if err := h.command("chown", "root:redis", target); err != nil {
				return err
			}
			if err := h.command("chmod", "0640", target); err != nil {
				return err
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(stage, "caddy")); err == nil {
		target := h.path("/var/lib/caddy/.local/share/caddy")
		// A fresh Caddy installation may not have created its TLS storage parents.
		// Give Caddy traversal to every new parent, not just the restored leaf tree.
		if err := h.command("install", "-d", "-o", "caddy", "-g", "caddy", "-m", "0700", "--", h.path("/var/lib/caddy/.local"), h.path("/var/lib/caddy/.local/share"), target); err != nil {
			return err
		}
		if err := backup.CopyTree(filepath.Join(stage, "caddy"), target); err != nil {
			return err
		}
		if err := h.command("chown", "-hR", "caddy:caddy", "--", target); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if !m.Settings.NoRedis && m.Scope != "app" {
		if err := h.command("systemctl", "stop", "redis-server"); err != nil {
			return err
		}
		target := h.path("/var/lib/redis")
		// Remove only Redis's freshly provisioned standard persistence artifacts.
		// Leaving setup's AOF beside the restored snapshot would hide restored data.
		for _, name := range []string{"dump.rdb", "appendonlydir"} {
			if err := os.RemoveAll(filepath.Join(target, name)); err != nil {
				return err
			}
		}
		if err := backup.CopyTree(filepath.Join(stage, "redis"), target); err != nil {
			return err
		}
		if err := h.command("chown", "-hR", "redis:redis", "--", target); err != nil {
			return err
		}
		if err := h.startRestoredRedis(); err != nil {
			return err
		}
	} else if !m.Settings.NoRedis && m.Scope == "app" {
		// The fresh Redis data stays empty, but saved runtime settings must take effect.
		if _, ok := m.Files["shared/redis.conf"]; ok {
			if err := h.command("systemctl", "restart", "redis-server"); err != nil {
				return err
			}
			if _, err := h.run("Verify fresh Redis", Command{Name: "redis-cli", Args: []string{"PING"}, Private: true}); err != nil {
				return err
			}
		}
	}
	if _, ok := m.Files["shared/mysql.cnf"]; ok {
		if err := h.command("/usr/sbin/mysqld", "--validate-config", "--user=mysql"); err != nil {
			return err
		}
		if err := h.command("systemctl", "restart", "mysql"); err != nil {
			return err
		}
	}
	if err := h.command("/usr/sbin/php-fpm"+services.PHPVersion, "--test"); err != nil {
		return err
	}
	if err := h.command("caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	return h.command("systemctl", "start", "caddy", "php"+services.PHPVersion+"-fpm")
}

// Online full backups carry an RDB, not a live AOF. Redis must first load that
// RDB with AOF disabled, then recreate its AOF from the loaded data.
func (h Host) startRestoredRedis() (result error) {
	name := "/etc/redis/abr.conf"
	data, err := h.read(name)
	if err != nil {
		return err
	}
	directive := regexp.MustCompile(`(?m)^[ \t]*appendonly[ \t]+(yes|no)[ \t]*$`)
	matches := directive.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		return fmt.Errorf("restoring Redis requires one managed appendonly setting")
	}
	enabled := string(matches[0][1]) == "yes"
	writeConfig := func(contents []byte) error {
		if err := h.write(name, contents, 0640); err != nil {
			return err
		}
		return h.command("chown", "root:redis", name)
	}
	if enabled {
		defer func() { result = errors.Join(result, writeConfig(data)) }()
		if err := writeConfig(directive.ReplaceAll(data, []byte("appendonly no"))); err != nil {
			return err
		}
	}
	if err := h.command("systemctl", "start", "redis-server"); err != nil {
		return err
	}
	if _, err := h.run("Verify restored Redis", Command{Name: "redis-cli", Args: []string{"PING"}, Private: true}); err != nil {
		return err
	}
	if enabled {
		if _, err := h.run("Recreate Redis AOF from restored snapshot", Command{Name: "redis-cli", Args: []string{"CONFIG", "SET", "appendonly", "yes"}, Private: true}); err != nil {
			return err
		}
	}
	return nil
}

func validateFullBackup(m backup.Manifest, stage string) error {
	if !regexp.MustCompile(`^(latest|[0-9]{4}\.[0-9]+\.[0-9]+)$`).MatchString(m.Settings.RoadRunnerVersion) {
		return fmt.Errorf("invalid RoadRunner setting in backup")
	}
	for _, record := range m.Apps {
		if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(record.Commit) {
			return fmt.Errorf("invalid backup source commit")
		}
		if !githubRepository.MatchString(record.Repository) || strings.HasPrefix(record.Branch, "-") || strings.ContainsAny(record.Branch, " ~^:?*[\\") || strings.Contains(record.Branch, "..") || strings.Contains(record.Branch, "@{") || strings.HasSuffix(record.Branch, ".lock") || strings.HasSuffix(record.Branch, "/") || strings.HasSuffix(record.Branch, ".") {
			return fmt.Errorf("invalid backup repository or branch")
		}
	}
	allowed := func(name string, directory bool) bool {
		if m.Scope == "app" && (name == "redis" || name == "caddy" || strings.HasPrefix(name, "redis/") || strings.HasPrefix(name, "caddy/")) {
			return false
		}
		if _, ok := sharedBackupFiles()[name]; ok {
			return !directory
		}
		if name == "shared" || name == "state" || name == "state/git" || name == "state/composer" || name == "state/databases" || name == "sql" || name == "apps" {
			return directory
		}
		for _, n := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json", "mysql-admin.json"} {
			if name == "state/"+n {
				return !directory
			}
		}
		if name == "templates" || name == "redis" || name == "caddy" {
			return directory
		}
		if strings.HasPrefix(name, "templates/") {
			return !directory && filepath.Dir(name) == "templates" && strings.HasSuffix(name, ".tmpl")
		}
		if strings.HasPrefix(name, "redis/") {
			return !m.Settings.NoRedis
		}
		if strings.HasPrefix(name, "caddy/") {
			return true
		}
		for _, a := range m.Config.Apps {
			if name == "state/databases/"+a.Name+".json" || name == "sql/"+a.Name+".sql" {
				return !directory
			}
			base := "apps/" + a.Name
			if name == base {
				return directory
			}
			if name == base+"/.env" {
				return !directory
			}
			if name == base+"/repository.bundle" {
				return !directory
			}
			if name == base+"/source-deleted.json" {
				return !directory
			}
			if name == base+"/source" {
				return directory
			}
			if strings.HasPrefix(name, base+"/source/") {
				return validSourcePath(strings.TrimPrefix(name, base+"/source/"))
			}
			if a.Type == "laravel" {
				if name == base+"/storage" || name == base+"/storage/app" {
					return directory
				}
				if strings.HasPrefix(name, base+"/storage/app/") {
					return true
				}
			}
		}
		return false
	}
	for name, record := range m.Files {
		if !record.Directory && (strings.HasPrefix(name, "state/") || strings.HasPrefix(name, "templates/") || strings.HasPrefix(name, "shared/") || strings.HasSuffix(name, "/.env")) && record.Size > 1<<20 {
			return fmt.Errorf("backup settings/secret file exceeds 1 MiB")
		}
		if !allowed(name, record.Directory) {
			return fmt.Errorf("unmanaged backup entry: %s", name)
		}
	}
	if len(m.Apps) == 0 {
		return fmt.Errorf("full backup has no apps")
	}
	if _, ok := m.Files["templates"]; !ok {
		return fmt.Errorf("backup templates missing")
	}
	if err := requireSetupTemplates(os.DirFS(filepath.Join(stage, "templates"))); err != nil {
		return err
	}
	for _, a := range m.Config.Apps {
		if record, ok := m.Files["apps/"+a.Name+"/repository.bundle"]; !ok || record.Directory || record.Size == 0 {
			return fmt.Errorf("%s: backup source bundle missing", a.Name)
		}
		if _, ok := m.Files["apps/"+a.Name+"/source"]; !ok {
			return fmt.Errorf("%s: backup working source missing", a.Name)
		}
		if record, ok := m.Files["apps/"+a.Name+"/source-deleted.json"]; !ok || record.Directory || record.Size > 1<<20 {
			return fmt.Errorf("%s: backup source deletion inventory missing or too large", a.Name)
		}
		data, err := readProjectFile(filepath.Join(stage, "apps", a.Name, "source-deleted.json"), 1<<20)
		if err != nil {
			return err
		}
		var deleted []string
		if err := json.Unmarshal(data, &deleted); err != nil {
			return fmt.Errorf("invalid source deletion inventory")
		}
		for _, name := range deleted {
			if !validSourcePath(name) {
				return fmt.Errorf("invalid deleted-source path")
			}
		}
		if _, err := services.Render(a, m.Ports, filepath.Join(stage, "templates"), "/var/lib/abr"); err != nil {
			return err
		}
	}
	if data, err := readProjectFile(filepath.Join(stage, "state/composer/auth.json"), 64<<10); err == nil {
		var auth composerAuth
		if json.Unmarshal(data, &auth) != nil || len(auth.HTTPBasic) == 0 {
			return fmt.Errorf("invalid backup Composer credentials")
		}
		for repository, login := range auth.HTTPBasic {
			if !composerRepository.MatchString(repository) || strings.TrimSpace(login.Username) == "" || login.Password == "" {
				return fmt.Errorf("invalid backup Composer credentials")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"state/git/id_ed25519", "state/git/known_hosts"} {
		if record, ok := m.Files[name]; !ok || record.Size == 0 || record.Size > 64<<10 {
			return fmt.Errorf("backup Git credentials missing or too large")
		}
	}
	if !m.Settings.NoRedis && m.Scope != "app" {
		if _, ok := m.Files["redis/dump.rdb"]; !ok {
			return fmt.Errorf("backup Redis snapshot missing")
		}
	}
	for _, a := range m.Config.Apps {
		if a.Type == "laravel" {
			if _, ok := m.Files["apps/"+a.Name+"/.env"]; !ok {
				return fmt.Errorf("%s: backup .env missing", a.Name)
			}
		}
		credentialName := "state/databases/" + a.Name + ".json"
		_, hasDB := m.Files[credentialName]
		sql, hasSQL := m.Files["sql/"+a.Name+".sql"]
		if hasDB != hasSQL || ((a.Database.Enabled || a.Type == "laravel") && !hasDB) || (hasSQL && sql.Size == 0) {
			return fmt.Errorf("%s: backup database files missing or incomplete", a.Name)
		}
		if hasDB {
			data, err := os.ReadFile(filepath.Join(stage, credentialName))
			if err != nil {
				return err
			}
			var c credentials
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&c); err != nil {
				return fmt.Errorf("invalid backup database credentials")
			}
			if decoder.Decode(new(any)) != io.EOF || c.App != a.Name || c.Database != databaseName(a.Name) || c.User != databaseUser(a.Name) || !c.Ready || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
				return fmt.Errorf("invalid backup database ownership record")
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(stage, "state/mysql-admin.json")); err == nil {
		var c mysqlAdminCredentials
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return fmt.Errorf("invalid backup admin credentials")
		}
		if decoder.Decode(new(any)) != io.EOF || c.User != "root" || c.Host != "127.0.0.1" || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
			return fmt.Errorf("invalid backup admin ownership record")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (h Host) copyBackupTemplates(destination string) error {
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(h.path(h.TemplatesDir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tmpl") {
			continue
		}
		if err := copyBackupFile(h.path(filepath.Join(h.TemplatesDir, entry.Name())), filepath.Join(destination, entry.Name()), false); err != nil {
			return err
		}
	}
	return nil
}

// Refuse even in-project links: a private-upload link into public assets would
// expose restored private files despite remaining inside the project root.
func openRestoreDirectory(root *os.Root, name string) (*os.Root, error) {
	prefix := ""
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		prefix = filepath.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if os.IsNotExist(err) {
			if err := root.Mkdir(prefix, 0700); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else if !info.IsDir() {
			return nil, fmt.Errorf("upload restore paths must be directories without symlinks")
		}
	}
	return root.OpenRoot(name)
}

// Saved ownership records are not evidence of ownership on a new host. Check
// every managed SQL identity before writing those records or importing any dump.
func (h Host) restoreDatabasePreflight(m backup.Manifest) error {
	for _, a := range m.Config.Apps {
		if _, ok := m.Files["state/databases/"+a.Name+".json"]; !ok {
			continue
		}
		query := fmt.Sprintf("SELECT (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='%s') + (SELECT COUNT(*) FROM mysql.user WHERE User='%s');\n", databaseName(a.Name), databaseUser(a.Name))
		out, err := h.mysql([]byte(query))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "0" {
			return fmt.Errorf("restore database or MySQL account for %s already exists; existing data and credentials preserved", a.Name)
		}
	}
	return nil
}
