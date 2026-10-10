package host

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Only temporary repository commands execute; host/service commands stay mocked.
type fixtureGitRunner struct{ base Runner }

func (r fixtureGitRunner) Run(c Command) ([]byte, error) {
	index := slices.Index(c.Args, "git")
	if c.Name != "runuser" || index < 0 {
		return r.base.Run(c)
	}
	command := exec.Command("git", c.Args[index+1:]...)
	command.Dir = c.Dir
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if c.Stdout != nil {
		command.Stdout = c.Stdout
		return nil, command.Run()
	}
	return command.Output()
}

func TestSavedSourceBundleIncludesWorkingChangesAndStagedDeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	a, _, err := h.application("app")
	if err != nil {
		t.Fatal(err)
	}
	git := func(directory string, args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=Backup fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
		command.Dir = directory
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git failed: %v %s", err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(a.Directory, "deleted.txt"), []byte("old tracked source"), 0600); err != nil {
		t.Fatal(err)
	}
	git(a.Directory, "init", "--initial-branch=main")
	git(a.Directory, "add", "artisan", "saved.txt", "deleted.txt")
	git(a.Directory, "commit", "--no-gpg-sign", "-m", "Backup source fixture")
	git(a.Directory, "remote", "add", "origin", "git@github.com:fixture/app.git")
	git(a.Directory, "rm", "deleted.txt")
	if err := os.WriteFile(filepath.Join(a.Directory, "saved.txt"), []byte("saved local modification"), 0600); err != nil {
		t.Fatal(err)
	}
	h.Runner = fixtureGitRunner{base: runner}
	if err := h.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	m, stage := readFullArchive(t, output)
	project := filepath.Join(h.root, "offline-checkout")
	git(h.root, "clone", "--branch", "main", "--", filepath.Join(stage, "apps/app/repository.bundle"), project)
	a.Directory = project
	if err := h.restoreWorkingSource(stage, a, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatal("staged deletion not restored")
	}
	if data, err := os.ReadFile(filepath.Join(project, "saved.txt")); err != nil || string(data) != "saved local modification" {
		t.Fatal("working change not restored", err)
	}
}

type deletedSourceRunner struct{ base Runner }

func (r deletedSourceRunner) Run(c Command) ([]byte, error) {
	data, err := r.base.Run(c)
	if err == nil && c.Name == "runuser" && slices.Contains(c.Args, "ls-tree") {
		data = append(data, []byte("deleted.txt\x00")...)
	}
	return data, err
}

func TestBackupWorkingSourceConfigurationDeletionsAndExecutable(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	a, _, err := h.application("app")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{".npmrc": "//packages.example.invalid/:_authToken=fixture-secret\n", "local.ini": "ignored local config", "bin/tool": "#!/bin/sh\n"} {
		path := filepath.Join(a.Directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	h.Runner = deletedSourceRunner{base: runner}
	if err := h.FullBackup(output); err != nil {
		t.Fatal(err)
	}
	m, stage := readFullArchive(t, output)
	if m.Apps[0].Commit != strings.Repeat("a", 40) {
		t.Fatal("source commit not retained")
	}
	for _, name := range []string{".npmrc", "local.ini", "bin/tool"} {
		if _, ok := m.Files["apps/app/source/"+name]; !ok {
			t.Fatal("working file missing", name)
		}
	}
	deleted, err := os.ReadFile(filepath.Join(stage, "apps/app/source-deleted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory []string
	if err := json.Unmarshal(deleted, &inventory); err != nil || !slices.Contains(inventory, "deleted.txt") {
		t.Fatal("source deletion lost", err)
	}
	project := filepath.Join(h.root, "restored-source")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "deleted.txt"), []byte("old tracked file"), 0600); err != nil {
		t.Fatal(err)
	}
	a.Directory = project
	if err := h.restoreWorkingSource(stage, a, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted tracked file reappeared")
	}
	info, err := os.Stat(filepath.Join(project, "bin/tool"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("source executable bits lost", err)
	}
	if data, err := os.ReadFile(filepath.Join(project, "local.ini")); err != nil || string(data) != "ignored local config" {
		t.Fatal("local config lost", err)
	}
}

func TestBackupRefusesUnsupportedSourceAndChangedCommit(t *testing.T) {
	for _, mode := range []string{"symlink", "submodule", "commit"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, output, cleanup := fullFixture(t, false)
			defer cleanup()
			directory := filepath.Join(h.AppsDir, "app")
			if mode == "symlink" {
				if err := os.Symlink("saved.txt", filepath.Join(directory, "linked-source")); err != nil {
					t.Fatal(err)
				}
			} else if mode == "submodule" {
				if err := os.WriteFile(filepath.Join(directory, ".gitmodules"), []byte("submodule fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				count := 0
				h.Runner = transferRunner{func(c Command) ([]byte, error) {
					if c.Name == "runuser" && slices.Contains(c.Args, "rev-parse") {
						count++
						if count > 1 {
							return []byte(strings.Repeat("b", 40)), nil
						}
					}
					return runner.Run(c)
				}}
			}
			if err := h.FullBackup(output); err == nil {
				t.Fatal("unsafe/changing source capture succeeded")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("incomplete archive published")
			}
		})
	}
}

func TestCopyIntoRootRejectsNestedLinks(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "private/secret"), []byte("private data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("public", filepath.Join(target, "private")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := copyIntoRoot(source, root); err == nil {
		t.Fatal("private data followed nested symlink")
	}
	if _, err := os.Stat(filepath.Join(target, "public/secret")); !os.IsNotExist(err) {
		t.Fatal("secret leaked into public")
	}
}

func TestRedisRestoreLoadsRDBBeforeRecreatingAOF(t *testing.T) {
	h, runner, _, cleanup := fullFixture(t, false)
	defer cleanup()
	configPath := h.path("/etc/redis/abr.conf")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	started, recreated := false, false
	runner.onCommand = func(c Command) error {
		if c.Name == "systemctl" && slices.Contains(c.Args, "start") && slices.Contains(c.Args, "redis-server") {
			data, err := os.ReadFile(configPath)
			if err != nil || strings.Contains(string(data), "appendonly yes") {
				t.Fatal("Redis started with AOF enabled before loading RDB", err)
			}
			started = true
		}
		if c.Name == "redis-cli" && slices.Contains(c.Args, "CONFIG") {
			if !started {
				t.Fatal("AOF enabled before Redis loaded snapshot")
			}
			recreated = true
		}
		return nil
	}
	if err := h.startRestoredRedis(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) || !started || !recreated {
		t.Fatal("Redis settings/AOF not restored", err)
	}
}
