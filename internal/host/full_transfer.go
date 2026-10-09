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

func copyBackupTree(source, destination string, optional bool) error {
	info, err := os.Lstat(source)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("backup source must be a directory without symlinks: %s", source)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(source) {
		return fmt.Errorf("backup directory parents must not be symlinks")
	}
	return backup.CopyTree(source, destination)
}

func (h Host) serviceActive(unit string) (bool, error) {
	out, err := h.run("Read shared service state", Command{Name: "systemctl", Args: []string{"show", "--property=ActiveState", "--value", unit}, Private: true})
	if err != nil {
		return false, err
	}
	state := strings.TrimSpace(string(out))
	if state != "active" && state != "inactive" && state != "failed" {
		return false, fmt.Errorf("shared service %s is not in a stable state", unit)
	}
	return state == "active", nil
}

// FullBackup produces one complete, root-only archive. It publishes only after
// every capture, checksum and service restart has succeeded.
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
			h.say("Would pause %d managed apps, capture MySQL, Redis, .env, Laravel public/private uploads, settings, templates and credentials into %s, then resume previously enabled apps; no files changed", len(c.Apps), output)
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
			// Validate all shared credentials and templates before taking services down.
			for _, name := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json", "mysql-admin.json"} {
				if err := copyBackupFile(h.path(filepath.Join(h.Manager.StateDir, name)), filepath.Join(stage, "state", name), name != "git/id_ed25519" && name != "git/known_hosts"); err != nil {
					return err
				}
			}
			if err := h.copyBackupTemplates(filepath.Join(stage, "templates")); err != nil {
				return err
			}
			sharedUnits := []string{"caddy", "php" + services.PHPVersion + "-fpm"}
			if !settings.NoRedis {
				sharedUnits = append(sharedUnits, "redis-server")
			}
			active := map[string]bool{}
			for _, unit := range sharedUnits {
				active[unit], err = h.serviceActive(unit)
				if err != nil {
					return err
				}
			}
			if !settings.NoRedis && !active["redis-server"] {
				return fmt.Errorf("managed Redis must be running before a full backup")
			}
			var paused []config.App
			var stopped []string
			resumed := false
			resume := func() error {
				var failures []error
				// Shared services must be ready before app enable/reload operations.
				for _, unit := range slices.Backward(stopped) {
					if active[unit] {
						if err := h.command("systemctl", "start", unit); err != nil {
							failures = append(failures, err)
						}
					}
				}
				for _, a := range paused {
					index := slices.IndexFunc(records, func(record backup.App) bool { return record.Name == a.Name })
					if index >= 0 && records[index].Enabled {
						if err := h.enable(a, r); err != nil {
							failures = append(failures, fmt.Errorf("resume %s: %w", a.Name, err))
						}
					}
				}
				return errors.Join(failures...)
			}
			defer func() {
				if !resumed {
					result = errors.Join(result, resume())
				}
			}()
			for _, a := range c.Apps {
				paused = append(paused, a)
				if err := h.disable(a); err != nil {
					return err
				}
			}
			for _, unit := range sharedUnits {
				if unit == "redis-server" {
					if _, err := h.run("Persist Redis snapshot", Command{Name: "redis-cli", Args: []string{"SAVE"}, Private: true}); err != nil {
						return err
					}
				}
				stopped = append(stopped, unit)
				if err := h.command("systemctl", "stop", unit); err != nil {
					return err
				}
			}
			for _, a := range c.Apps {
				_, err := h.run("Check remaining app writers", Command{Name: "pgrep", Args: []string{"-u", a.User}, Private: true})
				var status interface{ ExitCode() int }
				if err == nil {
					return fmt.Errorf("%s still has processes; stop external app writers before backing up", a.User)
				}
				if !errors.As(err, &status) || status.ExitCode() != 1 {
					return err
				}
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
				if err := copyBackupFile(h.path(filepath.Join(a.Directory, ".env")), filepath.Join(stage, "apps", a.Name, ".env"), a.Type == "nuxt"); err != nil {
					return err
				}
				if a.Type == "laravel" {
					for _, kind := range []string{"public", "private"} {
						if err := copyBackupTree(h.path(filepath.Join(a.Directory, "storage/app", kind)), filepath.Join(stage, "apps", a.Name, "storage/app", kind), true); err != nil {
							return err
						}
					}
				}
			}
			for name, source := range sharedBackupFiles() {
				if err := copyBackupFile(h.path(source), filepath.Join(stage, name), true); err != nil {
					return err
				}
			}
			if !settings.NoRedis {
				if err := copyBackupTree(h.path("/var/lib/redis"), filepath.Join(stage, "redis"), false); err != nil {
					return err
				}
			}
			if err := copyBackupTree(h.path("/var/lib/caddy/.local/share/caddy"), filepath.Join(stage, "caddy"), true); err != nil {
				return err
			}
			resumed = true
			if err := resume(); err != nil {
				return fmt.Errorf("backup captured but app restart failed; archive not published: %w", err)
			}
			f, err := os.CreateTemp(filepath.Dir(output), ".abr-backup-")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			defer f.Close()
			m := backup.Manifest{Version: 1, CreatedAt: time.Now().UTC(), Config: c, Ports: r, Apps: records, Settings: settings}
			if err := backup.Write(f, stage, &m); err != nil {
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
	if !o.Yes && !h.DryRun {
		return fmt.Errorf("full restore imports SQL and Redis data; pass --yes to confirm")
	}
	if !filepath.IsAbs(input) || !strings.HasSuffix(input, ".tar.gz") || strings.ContainsAny(input, "\x00\r\n") {
		return fmt.Errorf("use an absolute backup FILE.tar.gz")
	}
	if o.SSHPort < 0 || o.SSHPort > 65535 {
		return fmt.Errorf("invalid SSH port")
	}
	if err := h.guard(); err != nil {
		return err
	}
	f, err := os.OpenFile(input, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup must be a regular file")
	}
	// A preview extracts only to an ephemeral private directory, never host paths.
	stage, err := os.MkdirTemp("", "abr-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	m, err := backup.Extract(f, stage)
	if err != nil {
		return fmt.Errorf("invalid full backup: %w", err)
	}
	if err := validateFullBackup(m, stage); err != nil {
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
		h.say("Restore preview: %d apps; hostname %s; MySQL dumps and credentials; Redis: %t; latest commits from saved branches", len(m.Apps), m.Settings.Hostname, !m.Settings.NoRedis)
		for _, record := range m.Apps {
			h.say("Would clone latest %s branch %s into %s, restore .env/uploads/SQL, install dependencies and build with saved settings, migrate and optimize", record.Repository, record.Branch, filepath.Join(h.AppsDir, record.Name))
		}
		h.say("Would provision Ubuntu shared runtimes, recreate users/accounts, regenerate services and ports, restore Redis and Caddy data, then enable previously enabled apps; no host changes")
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

// Shared runtimes and the empty-target snapshot are installed before this phase.
func (h Host) restoreApplications(m backup.Manifest, stage string) (result error) {
	// All new apps stay stopped until every build/import succeeds.
	defer func() {
		if result != nil {
			var stopErrors []error
			for _, a := range m.Config.Apps {
				if err := h.disable(a); err != nil {
					stopErrors = append(stopErrors, err)
				}
			}
			result = errors.Join(result, errors.Join(stopErrors...))
			if len(stopErrors) == 0 {
				h.say("Full restore failed; restored app services are stopped. Partial data is retained for inspection.")
			} else {
				h.say("Full restore failed; some apps could not be stopped. Check service status. Partial data is retained for inspection.")
			}
		}
	}()
	for _, name := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json"} {
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
	for _, a := range m.Config.Apps {
		record := m.Apps[slices.IndexFunc(m.Apps, func(record backup.App) bool { return record.Name == a.Name })]
		if err := h.Clone(record.Repository, a.Directory); err != nil {
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
		if err := restoreAppFiles(stage, a, h); err != nil {
			return err
		}
		dbPath := filepath.Join(stage, "state/databases", a.Name+".json")
		if data, err := os.ReadFile(dbPath); err == nil {
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
			if err := h.importDatabaseFile(dbApp, filepath.Join(stage, "sql", a.Name+".sql")); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := h.restoreSharedData(stage, m); err != nil {
		return err
	}
	for _, a := range m.Config.Apps {
		if err := h.deploy(a, m.Ports, DeployOptions{NoPull: true, deferEnable: true}); err != nil {
			return fmt.Errorf("restore deployment %s: %w", a.Name, err)
		}
	}
	for _, a := range m.Config.Apps {
		record := m.Apps[slices.IndexFunc(m.Apps, func(record backup.App) bool { return record.Name == a.Name })]
		if record.Enabled {
			if err := h.enable(a, m.Ports); err != nil {
				return err
			}
			if err := h.deploymentHealth(a); err != nil {
				return err
			}
		}
	}
	h.say("Full restore complete: %d apps restored from latest Git branches. Check app health and point DNS at this server; keep the old server's workers stopped.", len(m.Apps))
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
		for _, name := range []string{a.Directory, "/var/lib/abr-users/" + a.User} {
			if _, err := os.Lstat(h.path(name)); !os.IsNotExist(err) {
				return fmt.Errorf("restore target already exists: %s", name)
			}
		}
		if hostChecks {
			if _, exists, err := h.passwd(a.User); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("restore user %s already exists", a.User)
			}
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
		for _, kind := range []string{"public", "private"} {
			source := filepath.Join(stage, "apps", a.Name, "storage/app", kind)
			if _, err := os.Lstat(source); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
			projectRoot, err := os.OpenRoot(h.path(a.Directory))
			if err != nil {
				return err
			}
			target := filepath.Join("storage/app", kind)
			var targetRoot *os.Root
			targetRoot, err = openRestoreDirectory(projectRoot, target)
			if err == nil {
				err = copyIntoRoot(source, targetRoot)
				targetRoot.Close()
			}
			projectRoot.Close()
			if err != nil {
				return err
			}
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
			return target.MkdirAll(rel, 0700)
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		out, err := target.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
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
	if !m.Settings.NoRedis {
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
		if err := h.command("systemctl", "start", "redis-server"); err != nil {
			return err
		}
		if _, err := h.run("Verify restored Redis", Command{Name: "redis-cli", Args: []string{"PING"}, Private: true}); err != nil {
			return err
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

func validateFullBackup(m backup.Manifest, stage string) error {
	if !regexp.MustCompile(`^(latest|[0-9]{4}\.[0-9]+\.[0-9]+)$`).MatchString(m.Settings.RoadRunnerVersion) {
		return fmt.Errorf("invalid RoadRunner setting in backup")
	}
	for _, record := range m.Apps {
		if !githubRepository.MatchString(record.Repository) || strings.HasPrefix(record.Branch, "-") || strings.ContainsAny(record.Branch, " ~^:?*[\\") || strings.Contains(record.Branch, "..") || strings.Contains(record.Branch, "@{") || strings.HasSuffix(record.Branch, ".lock") || strings.HasSuffix(record.Branch, "/") || strings.HasSuffix(record.Branch, ".") {
			return fmt.Errorf("invalid backup repository or branch")
		}
	}
	allowed := func(name string, directory bool) bool {
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
			if a.Type == "laravel" {
				if name == base+"/storage" || name == base+"/storage/app" {
					return directory
				}
				for _, kind := range []string{"public", "private"} {
					prefix := base + "/storage/app/" + kind
					if name == prefix {
						return directory
					}
					if strings.HasPrefix(name, prefix+"/") {
						return true
					}
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
	if !m.Settings.NoRedis {
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
