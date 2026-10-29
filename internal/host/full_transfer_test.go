package host

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"abr"
	"abr/internal/backup"
	"abr/internal/config"
	"abr/internal/manager"
	"abr/internal/ports"
	"abr/internal/services"
)

type fullRunner struct {
	base        *fakeRunner
	failDump    bool
	cloneSource string
	freshRoot   string
	onCommand   func(Command) error
}

func (r *fullRunner) Run(c Command) ([]byte, error) {
	if r.onCommand != nil {
		if err := r.onCommand(c); err != nil {
			return nil, err
		}
	}
	if c.Name == "runuser" && slices.Contains(c.Args, "clone") && r.cloneSource != "" {
		r.base.calls = append(r.base.calls, c)
		return nil, backup.CopyTree(r.cloneSource, c.Args[len(c.Args)-1])
	}
	if c.Name == "useradd" && r.freshRoot != "" {
		out, err := r.base.Run(c)
		if err != nil {
			return out, err
		}
		index := slices.Index(c.Args, "--home-dir")
		if index >= 0 {
			err = os.MkdirAll(filepath.Join(r.freshRoot, c.Args[index+1]), 0700)
		}
		return out, err
	}
	if c.Name == "runuser" && slices.Contains(c.Args, "git") {
		if slices.Contains(c.Args, "bundle") && slices.Contains(c.Args, "create") {
			r.base.calls = append(r.base.calls, c)
			_, err := io.WriteString(c.Stdout, "fixture self-contained source bundle\n")
			return nil, err
		}
		if slices.Contains(c.Args, "ls-tree") {
			r.base.calls = append(r.base.calls, c)
			var names []string
			err := filepath.WalkDir(c.Dir, func(name string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() {
					if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "node_modules" {
						return filepath.SkipDir
					}
					return nil
				}
				rel, err := filepath.Rel(c.Dir, name)
				if err == nil {
					names = append(names, filepath.ToSlash(rel))
				}
				return err
			})
			return []byte(strings.Join(names, "\x00") + "\x00"), err
		}
		if slices.Contains(c.Args, "remote") && slices.Contains(c.Args, "get-url") {
			r.base.calls = append(r.base.calls, c)
			return []byte("git@github.com:fixture/" + filepath.Base(c.Dir) + ".git\n"), nil
		}
		if slices.Contains(c.Args, "symbolic-ref") {
			r.base.calls = append(r.base.calls, c)
			return []byte("main\n"), nil
		}
	}
	if c.Name == "mysqldump" {
		r.base.calls = append(r.base.calls, c)
		if _, err := io.WriteString(c.Stdout, "-- private SQL fixture\nCREATE TABLE backup_probe (id INT);\n"); err != nil {
			return nil, err
		}
		if r.failDump {
			return nil, errors.New("fixture dump failure")
		}
		return nil, nil
	}
	if c.Name == "ssh-keygen" && slices.Contains(c.Args, "-F") {
		r.base.calls = append(r.base.calls, c)
		return (ExecRunner{}).Run(c)
	}
	if c.Name == "redis-cli" && len(c.Args) == 2 && c.Args[0] == "--rdb" {
		r.base.calls = append(r.base.calls, c)
		return nil, os.WriteFile(c.Args[1], []byte("redis fixture snapshot"), 0600)
	}
	return r.base.Run(c)
}

func fullFixture(t *testing.T, enabled bool) (Host, *fullRunner, string, func()) {
	t.Helper()
	h, runner, _, a := fixture(t)
	// Unix socket paths have a short platform limit. Keep this disposable host short.
	previous := h.root
	short, err := os.MkdirTemp("/tmp", "abr-full-")
	if err != nil {
		t.Fatal(err)
	}
	short, err = filepath.EvalSymlinks(short)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(short); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(previous, short); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	h.root = short
	h.AppsDir = strings.Replace(h.AppsDir, previous, short, 1)
	h.Manager.ConfigPath = strings.Replace(h.Manager.ConfigPath, previous, short, 1)
	h.Manager.StateDir = strings.Replace(h.Manager.StateDir, previous, short, 1)
	a.Directory = strings.Replace(a.Directory, previous, short, 1)
	h.TemplatesDir = filepath.Join(h.root, "templates")
	templates, err := abr.TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.installTemplates(templates); err != nil {
		t.Fatal(err)
	}
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"state/git/id_ed25519":                                  "private fixture key",
		"state/git/known_hosts":                                 githubHostKey,
		"apps/app/storage/app/public/photo.txt":                 "public fixture upload",
		"apps/app/storage/app/private/private.txt":              "private fixture upload",
		"apps/app/storage/app/custom/data.bin":                  "custom storage fixture",
		"apps/app/saved.txt":                                    "saved source fixture",
		"apps/app/vendor/not-backed-up":                         "generated dependency",
		"var/lib/caddy/.local/share/caddy/pki/root.key":         "private fixture Caddy key",
		"var/lib/redis/dump.rdb":                                "redis fixture snapshot",
		"var/lib/redis/appendonlydir/appendonly.aof.1.base.rdb": "redis fixture AOF",
		"etc/redis/abr.conf":                                    "# abr managed\nappendonly yes\n",
	} {
		path := filepath.Join(h.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, FPM: true, Enabled: enabled}); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {}
	if enabled {
		socket := h.path("/run/php/abr-app.sock")
		if err := os.MkdirAll(filepath.Dir(socket), 0755); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		cleanup = func() { listener.Close() }
	}
	full := &fullRunner{base: runner}
	h.Runner = full
	return h, full, filepath.Join(h.root, "backups/full.tar.gz"), cleanup
}

func readFullArchive(t *testing.T, path string) (backup.Manifest, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// macOS temporary paths can include /var, a system symlink. Model the
	// canonical staging path required by the restore file checks.
	stage, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := backup.Extract(f, stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFullBackup(m, stage); err != nil {
		t.Fatal(err)
	}
	return m, stage
}

func TestFullBackupScopePrivateDataAndResume(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, true)
	defer cleanup()
	if err := h.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	m, stage := readFullArchive(t, output)
	if len(m.Apps) != 1 || !m.Apps[0].Enabled || m.Apps[0].Branch != "main" || m.Settings.NoRedis {
		t.Fatal("lost backup settings")
	}
	for name, want := range map[string]string{"apps/app/.env": "APP_KEY=existing-key\n", "apps/app/storage/app/public/photo.txt": "public fixture upload", "apps/app/storage/app/private/private.txt": "private fixture upload", "redis/dump.rdb": "redis fixture snapshot"} {
		data, err := os.ReadFile(filepath.Join(stage, name))
		if err != nil || string(data) != want {
			t.Fatal("backup lost data", name, err)
		}
	}
	for name := range m.Files {
		if strings.Contains(name, "vendor") || strings.Contains(name, "deployments") || strings.HasSuffix(name, "users/abr-app.json") {
			t.Fatal("backed up generated or host-specific data", name)
		}
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatal("archive exposes secrets")
	}
	app, _, err := h.application("app")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := h.loadManifest(app)
	if err != nil || !manifest.Enabled {
		t.Fatal("previously running app was not resumed", err)
	}
	capturedRedis := false
	for _, c := range runner.base.calls {
		if c.Name == "systemctl" {
			t.Fatal("online backup changed service state", c.Args)
		}
		if c.Name == "redis-cli" && slices.Contains(c.Args, "--rdb") {
			capturedRedis = true
		}
		if c.Name == "mysqldump" && (!c.Private || c.Stdout == nil) {
			t.Fatal("SQL not private/streamed")
		}
	}
	if !capturedRedis {
		t.Fatal("Redis online snapshot missing")
	}
	text := h.Output.(*bytes.Buffer).String()
	for _, secret := range []string{"private fixture key", "private SQL fixture", "APP_KEY=existing-key", "private fixture upload"} {
		if strings.Contains(text, secret) {
			t.Fatal("secret leaked to backup output")
		}
	}
	before, _ := os.ReadFile(output)
	if err := h.FullBackup(output); err == nil {
		t.Fatal("overwrote backup")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing backup changed")
	}
}

func TestFullBackupFailureDoesNotPublishOrChangeServices(t *testing.T) {
	for _, redisFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "dump", true: "redis"}[redisFailure], func(t *testing.T) {
			h, runner, output, cleanup := fullFixture(t, true)
			defer cleanup()
			runner.failDump = !redisFailure
			runner.onCommand = func(c Command) error {
				if c.Name == "systemctl" {
					t.Fatal("backup changed service state")
				}
				if redisFailure && c.Name == "redis-cli" {
					return errors.New("fixture Redis failure")
				}
				return nil
			}
			if err := h.FullBackup(output); err == nil {
				t.Fatal("backup failure reported success")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("published incomplete backup")
			}
			temps, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".abr-backup-*"))
			if len(temps) != 0 {
				t.Fatal("partial archive leaked")
			}
			a, _, _ := h.application("app")
			m, _, err := h.loadManifest(a)
			if err != nil || !m.Enabled {
				t.Fatal("failed capture changed app state", err)
			}
		})
	}
}

func TestFullBackupRejectsUnsafeUploadsAndOutputScope(t *testing.T) {
	h, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	if err := os.Symlink("/etc/passwd", filepath.Join(h.AppsDir, "app/storage/app/private/leak")); err != nil {
		t.Fatal(err)
	}
	if err := h.FullBackup(output); err == nil {
		t.Fatal("upload symlink accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("published unsafe backup")
	}
	if err := h.FullBackup(filepath.Join(h.Manager.StateDir, "backups/full.tar.gz")); err == nil {
		t.Fatal("archive inside state accepted")
	}
}

func freshFullHost(t *testing.T) Host {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	return Host{Manager: manager.Manager{ConfigPath: filepath.Join(root, "etc/config.toml"), StateDir: filepath.Join(root, "state"), Probe: func(int) error { return nil }}, AppsDir: filepath.Join(root, "apps"), TemplatesDir: filepath.Join(root, "templates"), root: root, check: func() error { return nil }, Output: &bytes.Buffer{}}
}

func TestFullRestoreConfirmationPreviewAndFreshTarget(t *testing.T) {
	source, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	if err := source.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	h := freshFullHost(t)
	if err := h.FullRestore(output, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatal("unconfirmed restore accepted", err)
	}
	h.DryRun = true
	if err := h.FullRestore(output, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Manager.StateDir); !os.IsNotExist(err) {
		t.Fatal("preview provisioned state")
	}
	text := h.Output.(*bytes.Buffer).String()
	if !strings.Contains(text, "saved code") || !strings.Contains(text, "migrate and optimize") {
		t.Fatal("missing restore preview", text)
	}
	if err := os.MkdirAll(filepath.Join(h.AppsDir, "app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.FullRestore(output, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("existing project accepted", err)
	}
	h = freshFullHost(t)
	h.DryRun = true
	h.Manager.Probe = func(int) error { return errors.New("occupied") }
	m, stage := readFullArchive(t, output)
	// A Nuxt reservation exercises saved-port validation even before provisioning.
	m.Config.Apps = []config.App{{Name: "web", Directory: "/srv/apps/web", User: "abr-web", Type: "nuxt", Domain: "web.test"}}
	m.Apps = []backup.App{{Name: "web", Repository: "git@github.com:fixture/web.git", Branch: "main"}}
	m.Ports.Assignments = append(m.Ports.Assignments, ports.Assignment{App: "web", Purpose: "nuxt-http", Port: 19001})
	if err := h.freshRestoreTarget(m, false); err == nil {
		t.Fatal("restore ignored port conflict")
	}
	m, _ = readFullArchive(t, output)
	m.Files["apps/app/source.go"] = backup.File{}
	if err := validateFullBackup(m, stage); err == nil {
		t.Fatal("unexpected app data accepted")
	}
}

func TestRestoreApplicationOrderingCredentialsAndBuildFailures(t *testing.T) {
	for _, order := range []string{config.BuildComposerFirst, config.BuildFrontendFirst} {
		for _, failBuild := range []bool{false, true} {
			t.Run(order+map[bool]string{false: "/success", true: "/failure"}[failBuild], func(t *testing.T) {
				source, _, output, cleanup := fullFixture(t, false)
				defer cleanup()
				if err := source.ComposerAuth("packages.example.invalid", "fixture", "fixture-token"); err != nil {
					t.Fatal(err)
				}
				a, _, err := source.application("app")
				if err != nil {
					t.Fatal(err)
				}
				creds, err := source.transferCredentials(a)
				if err != nil {
					t.Fatal(err)
				}
				env := setEnvValues([]byte("APP_KEY=existing-key\n"), [][2]string{{"DB_CONNECTION", "mysql"}, {"DB_HOST", "127.0.0.1"}, {"DB_PORT", "3306"}, {"DB_DATABASE", creds.Database}, {"DB_USERNAME", creds.User}, {"DB_PASSWORD", creds.Password}})
				if err := os.WriteFile(filepath.Join(a.Directory, ".env"), env, 0600); err != nil {
					t.Fatal(err)
				}
				fixtureBackupDestination(t, source)
				if err := source.FullBackup(output); err != nil {
					t.Fatal(err)
				}
				m, stage := readFullArchive(t, output)
				h := freshFullHost(t)
				m.Config.Apps[0].Directory = filepath.Join(h.AppsDir, "app")
				m.Config.Apps[0].BuildOrder = order
				if err := backup.CopyTree(filepath.Join(stage, "templates"), h.TemplatesDir); err != nil {
					t.Fatal(err)
				}
				if err := h.Manager.RestoreSnapshot(m.Config, m.Ports); err != nil {
					t.Fatal(err)
				}
				repository := t.TempDir()
				for _, name := range []string{"artisan", "composer.json", "composer.lock", "package.json", "package-lock.json", "public/index.php", "bootstrap/cache/.gitignore"} {
					path := filepath.Join(repository, name)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				// The clone models bundled committed source; the working-source overlay
				// must retain local edits/untracked files without pulling origin.
				os.WriteFile(filepath.Join(repository, "saved.txt"), []byte("committed source before local edit"), 0600)
				base := &fakeRunner{users: map[string]string{"_apt": "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"}}
				runner := &fullRunner{base: base, cloneSource: repository, freshRoot: h.root}
				imported := false
				runner.onCommand = func(c Command) error {
					if c.Name == "setfacl" && strings.HasSuffix(c.Args[len(c.Args)-1], "/composer/.htaccess") {
						guard := h.path(filepath.Join(h.composerDir(), ".htaccess"))
						data, err := os.ReadFile(guard)
						if err != nil || string(data) != "Deny from all\n" {
							t.Fatal("Composer access granted before restoring its home guard", err)
						}
						info, err := os.Stat(guard)
						if err != nil || info.Mode().Perm() != 0600 {
							t.Fatal("Composer home guard is not private", err)
						}
					}
					if c.Name == "mysql" && c.Stdin != nil {
						imported = true
						if !c.Private || strings.Contains(strings.Join(c.Args, " "), creds.Password) {
							t.Fatal("import exposed SQL/password")
						}
					}
					if c.Name == "runuser" && (slices.Contains(c.Args, "npm") || slices.Contains(c.Args, "composer")) {
						if !imported {
							t.Fatal("dependency scripts ran before SQL import")
						}
						restored, err := os.ReadFile(filepath.Join(h.AppsDir, "app/.env"))
						if err != nil || !bytes.Equal(restored, env) {
							t.Fatal("dependency scripts ran before exact .env restore")
						}
						if _, err := os.Stat(filepath.Join(h.AppsDir, "app/storage/app/public/photo.txt")); err != nil {
							t.Fatal("build ran before uploads restore")
						}
						if failBuild && slices.Contains(c.Args, "npm") && slices.Contains(c.Args, "build") {
							return errors.New("fixture build failure")
						}
					}
					return nil
				}
				h.Runner = runner
				err = h.locked(func() error { h.lockHeld = true; return h.restoreApplications(m, stage) })
				if failBuild && err == nil {
					t.Fatal("failed restore reported success")
				}
				if !failBuild && err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"backup-destination.json", "backup-ssh/id_ed25519", "backups/app.json"} {
					want, _ := os.ReadFile(filepath.Join(source.Manager.StateDir, name))
					if got, err := os.ReadFile(filepath.Join(h.Manager.StateDir, name)); err != nil || !bytes.Equal(got, want) {
						t.Fatal("backup destination or schedule time not restored", name, err)
					}
				}
				restored, err := h.transferCredentials(m.Config.Apps[0])
				if err != nil || restored.Password != creds.Password || !restored.TCPReady {
					t.Fatal("database identity/password not restored", err)
				}
				user, err := os.ReadFile(h.userPath(m.Config.Apps[0]))
				if err != nil || !bytes.Contains(user, []byte("abr-app")) {
					t.Fatal("user ownership record not recreated", err)
				}
				saved, err := os.ReadFile(filepath.Join(h.AppsDir, "app/saved.txt"))
				if err != nil || string(saved) != "saved source fixture" {
					t.Fatal("did not restore saved working source", err)
				}
				composerInstall, frontendBuild := -1, -1
				for i, c := range base.calls {
					if c.Name != "runuser" {
						continue
					}
					if slices.Contains(c.Args, "clone") && !strings.HasSuffix(c.Args[len(c.Args)-2], "repository.bundle") {
						t.Fatal("restore cloned remote source")
					}
					if slices.Contains(c.Args, "fetch") || slices.Contains(c.Args, "pull") {
						t.Fatal("restore fetched newer source")
					}
					if slices.Contains(c.Args, "composer") && slices.Contains(c.Args, "install") && !slices.Contains(c.Args, "--download-only") {
						composerInstall = i
					}
					if slices.Contains(c.Args, "npm") && slices.Contains(c.Args, "build") {
						frontendBuild = i
					}
				}
				if !failBuild && (composerInstall < 0 || frontendBuild < 0 || (composerInstall < frontendBuild) != (order == config.BuildComposerFirst)) {
					t.Fatal("saved build ordering ignored", composerInstall, frontendBuild)
				}
				if _, exists, err := h.loadManifest(m.Config.Apps[0]); err != nil || exists {
					t.Fatal("disabled app was enabled", err)
				}
				if strings.Contains(h.Output.(*bytes.Buffer).String(), creds.Password) {
					t.Fatal("restore logged password")
				}
			})
		}
	}
}

func TestFullBackupAllowsOnlineWriters(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	runner.base.processes = true
	if err := h.FullBackup(output); err != nil {
		t.Fatal("online capture refused active writers", err)
	}
	for _, c := range runner.base.calls {
		if c.Name == "pgrep" || c.Name == "systemctl" {
			t.Fatal("online capture inspected or changed services")
		}
	}
}

func TestFullTransferRejectsSymlinkedUploadParents(t *testing.T) {
	h, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	uploads := filepath.Join(h.AppsDir, "app/storage/app")
	elsewhere := filepath.Join(h.root, "other-uploads")
	if err := os.Rename(uploads, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, uploads); err != nil {
		t.Fatal(err)
	}
	if err := h.FullBackup(output); err == nil {
		t.Fatal("read uploads through a linked parent")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "storage/app/public"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("public", filepath.Join(project, "storage/app/private")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if target, err := openRestoreDirectory(root, "storage/app/private"); err == nil {
		target.Close()
		t.Fatal("private uploads could be restored into public assets")
	}
}

func TestRestoreRefusesExistingMySQLIdentity(t *testing.T) {
	h := freshFullHost(t)
	a := config.App{Name: "demo", User: "abr-demo", Directory: "/srv/apps/demo", Type: "laravel", Domain: "demo.test", Web: config.Web{Driver: "fpm"}, Database: config.Database{Enabled: true}}
	m := backup.Manifest{Config: config.Config{Apps: []config.App{a}}, Files: map[string]backup.File{"state/databases/demo.json": {}}}
	calls := 0
	h.Runner = transferRunner{func(c Command) ([]byte, error) {
		calls++
		if c.Name != "mysql" || !c.Private || c.Stdin != nil || len(c.Input) == 0 {
			t.Fatal("unsafe SQL preflight")
		}
		return []byte("1\n"), nil
	}}
	if err := h.restoreDatabasePreflight(m); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("adopted existing MySQL identity", err)
	}
	if calls != 1 {
		t.Fatal("database preflight was not executed")
	}
	if _, err := os.Stat(h.Manager.ConfigPath); !os.IsNotExist(err) {
		t.Fatal("database conflict changed config")
	}
	h.Runner = transferRunner{func(Command) ([]byte, error) { return []byte("0\n"), nil }}
	if err := h.restoreDatabasePreflight(m); err != nil {
		t.Fatal("empty MySQL target refused", err)
	}
}

func TestRestoreAppsIntoSetUpServer(t *testing.T) {
	source, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	addSecondBackupApp(t, source)
	a, _, err := source.application("app")
	if err != nil {
		t.Fatal(err)
	}
	original, err := source.transferCredentials(a)
	if err != nil {
		t.Fatal(err)
	}
	env := setEnvValues([]byte("APP_KEY=existing-key\nLOG_PATH="+a.Directory+"/storage/logs/app.log\n"), [][2]string{{"DB_CONNECTION", "mysql"}, {"DB_HOST", "127.0.0.1"}, {"DB_PORT", "3306"}, {"DB_DATABASE", original.Database}, {"DB_USERNAME", original.User}, {"DB_PASSWORD", original.Password}})
	if err := os.WriteFile(filepath.Join(a.Directory, ".env"), env, 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(output)
	if err := source.BackupApps(nil, BackupOptions{All: true, OutputDir: directory}); err != nil {
		t.Fatal(err)
	}
	current, _ := filepath.Glob(filepath.Join(directory, "app-*.tar.gz"))
	if len(current) != 1 {
		t.Fatal("app archive missing", current)
	}
	// An older generation of the same app must not be selected from the directory.
	older := filepath.Join(directory, "app-20000101000000.tar.gz")
	if err := os.Link(current[0], older); err != nil {
		t.Fatal(err)
	}
	saved, err := source.Manager.Registry()
	if err != nil {
		t.Fatal(err)
	}
	savedPort, ok := saved.Lookup("second", "nuxt-http")
	if !ok {
		t.Fatal("saved Nuxt port missing")
	}

	h := freshFullHost(t)
	templates, err := abr.TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.installTemplates(templates); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"setup.json": "{}\n", "git/id_ed25519": "target server key", "git/known_hosts": githubHostKey} {
		if err := h.write(filepath.Join(h.Manager.StateDir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// This server's own app already holds the saved port of the restored app.
	web := config.App{Name: "web", User: "abr-web", Directory: filepath.Join(h.AppsDir, "web"), Type: "nuxt", Domain: "web.localhost"}
	if _, err := h.Manager.Register(web, map[string]int{"nuxt-http": savedPort}); err != nil {
		t.Fatal(err)
	}

	h.DryRun = true
	if err := h.RestoreArchives([]string{directory}, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	text := h.Output.(*bytes.Buffer).String()
	if !strings.Contains(text, "into this server") || strings.Contains(text, filepath.Base(older)) || !strings.Contains(text, filepath.Base(current[0])) {
		t.Fatal("preview did not select the newest archives for this server", text)
	}
	if c, err := config.Load(h.Manager.ConfigPath); err != nil || len(c.Apps) != 1 {
		t.Fatal("preview changed configuration", err)
	}
	h.DryRun = false
	if err := h.RestoreArchives([]string{directory}, RestoreOptions{Yes: true, AdminUser: "root"}); err == nil || !strings.Contains(err.Error(), "only to a fresh server") {
		t.Fatal("fresh-server options accepted on a set-up server", err)
	}

	repository := t.TempDir()
	for _, name := range []string{"artisan", "composer.json", "composer.lock", "package.json", "package-lock.json", "public/index.php", "bootstrap/cache/.gitignore"} {
		path := filepath.Join(repository, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	base := &fakeRunner{users: map[string]string{"_apt": "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"}}
	runner := &fullRunner{base: base, cloneSource: repository, freshRoot: h.root}
	h.Runner = runner
	if err := h.RestoreArchives([]string{directory}, RestoreOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range base.calls {
		if c.Name == "apt-get" || c.Name == "hostnamectl" || c.Name == "redis-cli" {
			t.Fatal("app restore changed shared server setup", c.Name, c.Args)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.Manager.StateDir, "git/id_ed25519")); err != nil || string(data) != "target server key" {
		t.Fatal("replaced this server's Git key", err)
	}
	c, r, err := h.Manager.Snapshot()
	if err != nil || len(c.Apps) != 3 {
		t.Fatal("restored apps not registered", err)
	}
	if port, _ := r.Lookup("web", "nuxt-http"); port != savedPort {
		t.Fatal("existing app lost its port")
	}
	if port, ok := r.Lookup("second", "nuxt-http"); !ok || port == savedPort {
		t.Fatal("restored app took an occupied port", port)
	}
	restored, err := h.transferCredentials(c.Apps[1])
	if err != nil || restored.Password != original.Password || !restored.TCPReady {
		t.Fatal("database identity/password not restored", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.AppsDir, "app/storage/app/private/private.txt")); err != nil || string(data) != "private fixture upload" {
		t.Fatal("uploads not restored", err)
	}
	text = h.Output.(*bytes.Buffer).String()
	if !strings.Contains(text, "Kept this server's Git deploy key") || !strings.Contains(text, "App restore complete: 2 apps") || strings.Contains(text, original.Password) {
		t.Fatal("restore report inaccurate", text)
	}
	if err := h.RestoreArchives(current, RestoreOptions{Yes: true}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatal("restored over an existing app", err)
	}
	if err := source.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	if err := h.RestoreArchives([]string{output}, RestoreOptions{Yes: true}); err == nil || !strings.Contains(err.Error(), "fresh server") {
		t.Fatal("full-server backup restored into a set-up server", err)
	}

	// The same app restored under a new name beside the existing app.
	for name, options := range map[string]RestoreOptions{
		"one app backup":     {Yes: true, Name: "app-old"},
		"lowercase":          {Yes: true, Name: "App-Old"},
		"already registered": {Yes: true, Name: "web"},
	} {
		inputs := current
		if name == "one app backup" {
			inputs = []string{directory}
		}
		if err := h.RestoreArchives(inputs, options); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatal("invalid renamed restore accepted", options, err)
		}
	}
	if err := h.Manager.Edit("app", config.AppChanges{Values: config.App{Domain: "www.app.localhost"}, Fields: []string{"domain"}}, true); err != nil {
		t.Fatal(err)
	}
	existing, err := h.transferCredentials(c.Apps[1])
	if err != nil {
		t.Fatal(err)
	}
	existingEnv, err := os.ReadFile(filepath.Join(h.AppsDir, "app/.env"))
	if err != nil {
		t.Fatal(err)
	}
	h.Output = &bytes.Buffer{}
	h.DryRun = true
	if err := h.RestoreArchives(current, RestoreOptions{Name: "app-old"}); err != nil {
		t.Fatal(err)
	}
	if text := h.Output.(*bytes.Buffer).String(); !strings.Contains(text, "Restoring app as app-old") || !strings.Contains(text, "Would restore app-old") || !strings.Contains(text, filepath.Join(h.AppsDir, "app-old")) {
		t.Fatal("renamed preview inaccurate", text)
	}
	h.DryRun = false
	// This fake MySQL reports any database once one exists; the new identity is unused.
	base.database, base.tcp = false, false
	first := len(base.calls)
	if err := h.RestoreArchives(current, RestoreOptions{Yes: true, Name: "app-old"}); err != nil {
		t.Fatal(err)
	}
	c, r, err = h.Manager.Snapshot()
	if err != nil || len(c.Apps) != 4 {
		t.Fatal("renamed app not registered", err)
	}
	renamed, _, err := h.application("app-old")
	if err != nil || renamed.Directory != filepath.Join(h.AppsDir, "app-old") || renamed.User != "abr-app-old" || renamed.Domain != "app.localhost" {
		t.Fatal("renamed app settings wrong", renamed, err)
	}
	moved, err := h.transferCredentials(renamed)
	if err != nil || moved.App != "app-old" || moved.Database != "app_old" || moved.User != "app_old" || moved.Password != original.Password || !moved.TCPReady {
		t.Fatal("database identity not renamed", moved, err)
	}
	data, err := os.ReadFile(filepath.Join(h.AppsDir, "app-old/.env"))
	if err != nil || dotenvValue(data, "DB_DATABASE") != "app_old" || dotenvValue(data, "DB_USERNAME") != "app_old" || dotenvValue(data, "DB_PASSWORD") != original.Password || dotenvValue(data, "APP_KEY") != "existing-key" {
		t.Fatal("renamed .env not updated", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.AppsDir, "app-old/storage/app/private/private.txt")); err != nil || string(data) != "private fixture upload" {
		t.Fatal("renamed uploads not restored", err)
	}
	for _, path := range []string{h.credentialsEnvPath("app-old"), h.userPath(renamed)} {
		if _, err := os.Stat(h.path(path)); err != nil {
			t.Fatal("renamed app record missing", path, err)
		}
	}
	plan, err := services.Render(renamed, r, h.TemplatesDir, h.Manager.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range plan.Files {
		rest := strings.ReplaceAll(file.Path+"\n"+string(file.Data), "abr-app-old", "")
		if !strings.Contains(file.Path, "app-old") || strings.Contains(rest, "abr-app") || strings.Contains(rest, filepath.Join(h.AppsDir, "app")+"/") {
			t.Fatal("renamed services still use the old name", file.Path)
		}
	}
	// The existing app keeps its records, data and MySQL identity untouched.
	if kept, err := h.transferCredentials(c.Apps[1]); err != nil || kept != existing {
		t.Fatal("existing app database record changed", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.AppsDir, "app/.env")); err != nil || !bytes.Equal(data, existingEnv) {
		t.Fatal("existing app .env changed", err)
	}
	for _, call := range base.calls[first:] {
		text := string(call.Input) + " " + strings.Join(call.Args, " ")
		if strings.Contains(text, "`app`") || strings.Contains(text, "'app'@") || strings.Contains(text, "--user abr-app ") || strings.Contains(text, "--database=app ") || (call.Name == "useradd" && slices.Contains(call.Args, "abr-app")) {
			t.Fatal("renamed restore touched the existing app", call.Name, call.Args)
		}
	}
	text = h.Output.(*bytes.Buffer).String()
	if !strings.Contains(text, "Review LOG_PATH in app-old's .env") || !strings.Contains(text, "App restore complete: 1 apps") || strings.Contains(text, original.Password) {
		t.Fatal("renamed restore report inaccurate", text)
	}
}

func TestRenamedRestoreOnFreshServerAndEnvReview(t *testing.T) {
	source, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	if err := source.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	h := freshFullHost(t)
	h.DryRun = true
	if err := h.FullRestore(output, RestoreOptions{Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	text := h.Output.(*bytes.Buffer).String()
	if !strings.Contains(text, "Restoring app as renamed") || !strings.Contains(text, filepath.Join(h.AppsDir, "renamed")) || strings.Contains(text, filepath.Join(h.AppsDir, "app")+" ") {
		t.Fatal("fresh-server rename preview inaccurate", text)
	}
	if _, err := os.Stat(h.Manager.StateDir); !os.IsNotExist(err) {
		t.Fatal("preview provisioned state")
	}
	env := []byte("# LOG=/srv/apps/demo/x\nAPP_NAME=demo\nLOG_PATH=\"/srv/apps/demo/storage/logs\"\nexport CACHE=/srv/apps/demo\nOTHER=/srv/apps/demo-two/x\nDB_URL=mysql://demo@127.0.0.1/demo\nDATABASE_URL=\n")
	if keys := renamedEnvReview(env, []string{"/srv/apps/demo", "/srv/apps/demo/"}); strings.Join(keys, ",") != "LOG_PATH,CACHE,DB_URL" {
		t.Fatal("renamed .env review inaccurate", keys)
	}
}

const backupFixtureKnownHost = "[backup.example.invalid]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// fixtureBackupDestination saves a key-based backup destination, root's trust
// of its host key and the app's last scheduled backup on h.
func fixtureBackupDestination(t *testing.T, h Host) {
	t.Helper()
	for name, data := range map[string]string{
		filepath.Join(h.Manager.StateDir, "backup-destination.json"):   `{"host":"backup.example.invalid","user":"backup","directory":"/srv/backups","port":2222,"auth":"key"}` + "\n",
		filepath.Join(h.Manager.StateDir, "backup-ssh/id_ed25519"):     "private backup fixture key",
		filepath.Join(h.Manager.StateDir, "backup-ssh/id_ed25519.pub"): "ssh-ed25519 AAAA backup-fixture\n",
		filepath.Join(h.Manager.StateDir, "backups/app.json"):          `{"LastSuccess":"2026-10-10T02:00:00Z","LastAttempt":"2026-10-10T02:00:00Z"}` + "\n",
		// Only entries for the destination are saved; markers never are.
		h.path("/root/.ssh/known_hosts"): githubHostKey + "\n" + backupFixtureKnownHost + "\n@revoked [backup.example.invalid]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJm\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupDestinationAndScheduleTravelWithBackups(t *testing.T) {
	h, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	fixtureBackupDestination(t, h)
	saved := map[string][]byte{}
	for _, name := range []string{"backup-destination.json", "backup-ssh/id_ed25519", "backup-ssh/id_ed25519.pub", "backups/app.json"} {
		saved[name], _ = os.ReadFile(filepath.Join(h.Manager.StateDir, name))
	}
	if err := h.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	// The fixture backup server is unreachable, so the app archive stays local.
	if err := h.BackupApps([]string{"app"}, BackupOptions{OutputDir: filepath.Join(h.root, "app-backups")}); err == nil || !strings.Contains(err.Error(), "local backup retained") {
		t.Fatal("app backup did not use the saved destination", err)
	}
	archives, _ := filepath.Glob(filepath.Join(h.root, "app-backups/app-*.tar.gz"))
	if len(archives) != 1 {
		t.Fatal("app archive missing")
	}
	for _, archive := range []string{output, archives[0]} {
		m, stage := readFullArchive(t, archive)
		for name, want := range saved {
			if got, err := os.ReadFile(filepath.Join(stage, "state", name)); err != nil || !bytes.Equal(got, want) {
				t.Fatal("backup destination or schedule time not saved", archive, name, err)
			}
		}
		if got, err := os.ReadFile(filepath.Join(stage, "state/backup-ssh/known_hosts")); err != nil || string(got) != backupFixtureKnownHost+"\n" {
			t.Fatal("saved other or unsafe known_hosts entries", string(got), err)
		}
		fresh := freshFullHost(t)
		for range 2 {
			if err := fresh.restoreSharedCredentials(stage, false); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := os.ReadFile(fresh.path("/root/.ssh/known_hosts")); err != nil || string(got) != backupFixtureKnownHost+"\n" {
			t.Fatal("backup server trust not restored once", string(got), err)
		}
		// A set-up server keeps its own destination; a full restore never adopts one.
		kept := freshFullHost(t)
		if err := kept.write(filepath.Join(kept.Manager.StateDir, "backup-destination.json"), []byte("own destination\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := kept.freshRestoreTarget(m, false); err == nil || !strings.Contains(err.Error(), "backup-destination.json") {
			t.Fatal("full restore adopted an existing backup destination", err)
		}
		if err := kept.restoreSharedCredentials(stage, true); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(kept.Manager.StateDir, "backup-destination.json")); string(got) != "own destination\n" {
			t.Fatal("restore replaced this server's backup destination")
		}
		if _, err := os.Stat(kept.path("/root/.ssh/known_hosts")); !os.IsNotExist(err) {
			t.Fatal("kept destination trusted the saved backup server", err)
		}
		preview := freshFullHost(t)
		preview.DryRun = true
		if err := preview.RestoreArchives([]string{archive}, RestoreOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(preview.Output.(*bytes.Buffer).String(), "restore the saved backup destination") {
			t.Fatal("preview omitted the backup destination")
		}
		if err := backup.ReplaceFile(&m, stage, "state/backup-ssh/known_hosts", []byte("@cert-authority * ssh-ed25519 AAAA\n")); err != nil {
			t.Fatal(err)
		}
		if err := validateFullBackup(m, stage); err == nil {
			t.Fatal("accepted a known_hosts marker")
		}
		delete(m.Files, "state/backup-ssh/known_hosts")
		delete(m.Files, "state/backup-ssh/id_ed25519")
		if err := validateFullBackup(m, stage); err == nil || !strings.Contains(err.Error(), "SSH key missing") {
			t.Fatal("accepted a key destination without its key", err)
		}
	}
}
