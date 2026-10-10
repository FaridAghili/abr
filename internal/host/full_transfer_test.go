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
