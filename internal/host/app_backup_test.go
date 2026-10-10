package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"abr/internal/backup"
	"abr/internal/config"
)

func addSecondBackupApp(t *testing.T, h Host) {
	t.Helper()
	a := config.App{Name: "second", User: "abr-second", Directory: filepath.Join(h.AppsDir, "second"), Type: "nuxt", Domain: "second.localhost"}
	if err := os.MkdirAll(a.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAppBackupSequentialVerifiedCleanupAndMerge(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, true)
	defer cleanup()
	addSecondBackupApp(t, h)
	runner.base.calls = nil
	directory := filepath.Dir(output)
	remote := t.TempDir()
	var received []string
	runner.onCommand = func(c Command) error {
		if c.Name == "systemctl" || c.Name == "pgrep" || c.Name == "redis-cli" {
			t.Fatal("online app backup touched shared services", c.Name, c.Args)
		}
		if c.Name == "runuser" && c.Dir == filepath.Join(h.AppsDir, "second") {
			if len(received) != 1 {
				t.Fatal("next app started before transfer")
			}
			files, _ := filepath.Glob(filepath.Join(directory, "app-*.tar.gz"))
			if len(files) != 0 {
				t.Fatal("previous app archive not cleaned before next app")
			}
		}
		return nil
	}
	h.Runner = backupSSHRunner{base: runner, ssh: func(c Command) ([]byte, error) {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return nil, err
		}
		f := c.Stdin.(*os.File)
		name := filepath.Base(f.Name())
		if !regexp.MustCompile(`^(app|second)-[0-9]{14}\.tar\.gz$`).MatchString(name) {
			t.Fatal("archive lacks compact app/timestamp filename", name)
		}
		path := filepath.Join(remote, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, err
		}
		m, stage := readFullArchive(t, path)
		if m.Scope != "app" || len(m.Apps) != 1 {
			t.Fatal("not a scoped app archive")
		}
		if _, ok := m.Files["redis/dump.rdb"]; ok {
			t.Fatal("included shared persistence")
		}
		if m.Apps[0].Name == "app" {
			if data, _ := os.ReadFile(filepath.Join(stage, "apps/app/storage/app/private/private.txt")); string(data) != "private fixture upload" {
				t.Fatal("private uploads missing")
			}
		}
		received = append(received, path)
		hash := sha256.Sum256(data)
		return []byte("ABR_BACKUP_OK " + hex.EncodeToString(hash[:]) + " " + name + "\n"), nil
	}}
	if err := h.BackupApps(nil, BackupOptions{All: true, OutputDir: directory, Remote: "backup@fixture", RemoteDir: "/srv/backups/abr"}); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 {
		t.Fatal("not all apps transferred")
	}
	files, _ := filepath.Glob(filepath.Join(directory, "*.tar.gz"))
	if len(files) != 0 {
		t.Fatal("local archives remain")
	}
	stages, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, ".app-backup*"))
	if len(stages) != 0 {
		t.Fatal("staging data retained")
	}
	fresh := freshFullHost(t)
	fresh.DryRun = true
	if err := fresh.RestoreArchives(received, RestoreOptions{}); err != nil {
		t.Fatal("separate archives cannot be restored together", err)
	}
	if err := fresh.RestoreArchives([]string{received[0], received[0]}, RestoreOptions{}); err == nil {
		t.Fatal("duplicate app accepted")
	}
	if _, err := os.Stat(fresh.Manager.StateDir); !os.IsNotExist(err) {
		t.Fatal("preview created host state")
	}
	text := h.Output.(*bytes.Buffer).String()
	if strings.Contains(text, "private fixture key") || strings.Contains(text, "private SQL fixture") {
		t.Fatal("backup leaked private data")
	}
}

type backupSSHRunner struct {
	base Runner
	ssh  func(Command) ([]byte, error)
}

func (r backupSSHRunner) Run(c Command) ([]byte, error) {
	if c.Name == "ssh" || c.Name == "sshpass" {
		return r.ssh(c)
	}
	return r.base.Run(c)
}

func TestAppBackupFailedTransferRetainsArchiveAndStopsRun(t *testing.T) {
	for _, mode := range []string{"connection", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, output, cleanup := fullFixture(t, false)
			defer cleanup()
			addSecondBackupApp(t, h)
			runner.base.calls = nil
			h.Runner = backupSSHRunner{base: runner, ssh: func(c Command) ([]byte, error) {
				if mode == "connection" {
					return nil, errors.New("fixture SSH failure")
				}
				return []byte("ABR_BACKUP_OK wrong checksum\n"), nil
			}}
			err := h.BackupApps(nil, BackupOptions{All: true, OutputDir: filepath.Dir(output), Remote: "backup@fixture", RemoteDir: "/srv/backups/abr"})
			if err == nil || !strings.Contains(err.Error(), "local backup retained") {
				t.Fatal("failed transfer reported success", err)
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(output), "*.tar.gz"))
			if len(files) != 1 {
				t.Fatal("failed transfer deleted archive or proceeded to next app")
			}
			readFullArchive(t, files[0])
			for _, c := range runner.base.calls {
				if c.Dir == filepath.Join(h.AppsDir, "second") {
					t.Fatal("continued after failed transfer")
				}
			}
		})
	}
}

func TestBackupDestinationPasswordPrivateAndSSHDescriptor(t *testing.T) {
	h, runner, _, cleanup := fullFixture(t, false)
	defer cleanup()
	d := backup.Destination{Host: "192.0.2.10", User: "backup", Directory: "/srv/backups/abr", Port: 2222, Auth: "password", Password: "fixture SSH secret"}
	if err := h.ConfigureBackup(d, ""); err != nil {
		t.Fatal(err)
	}
	saved, err := h.loadBackupDestination()
	if err != nil || saved != d {
		t.Fatal("lost private destination settings", err)
	}
	info, err := os.Stat(h.backupDestinationPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("destination not private", err)
	}
	called := false
	h.Runner = backupSSHRunner{base: runner, ssh: func(c Command) ([]byte, error) {
		called = true
		if c.Name != "sshpass" || !slices.Contains(c.Args, "2222") || len(c.ExtraFiles) != 1 || !c.Private {
			t.Fatal("password not passed through private descriptor")
		}
		if strings.Contains(strings.Join(c.Args, " "), d.Password) || strings.Contains(strings.Join(c.Env, " "), d.Password) {
			t.Fatal("password in process arguments/environment")
		}
		data, err := io.ReadAll(c.ExtraFiles[0])
		if err != nil || string(data) != d.Password+"\n" {
			t.Fatal("password descriptor invalid", err)
		}
		return []byte("ABR_DESTINATION_OK\n"), nil
	}}
	if err := h.TestBackupDestination(); err != nil {
		t.Fatal(err)
	}
	if !called || strings.Contains(h.Output.(*bytes.Buffer).String(), d.Password) {
		t.Fatal("connection untested or secret logged")
	}
	files, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, ".ssh-password-*"))
	if len(files) != 0 {
		t.Fatal("temporary password retained")
	}
}

func TestAppBackupPreviewNoWritesOrCommands(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	runner.base.calls = nil
	h.DryRun = true
	if err := h.BackupApps(nil, BackupOptions{All: true, OutputDir: filepath.Dir(output), Remote: "backup@fixture", RemoteDir: "/srv/backups/abr"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.base.calls) != 0 {
		t.Fatal("preview executed host commands")
	}
	if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
		t.Fatal("preview created output directory")
	}
	if err := h.BackupApps([]string{"missing"}, BackupOptions{OutputDir: filepath.Dir(output)}); err == nil {
		t.Fatal("unknown app accepted")
	}
	if err := h.BackupApps([]string{"app", "app"}, BackupOptions{OutputDir: filepath.Dir(output)}); err == nil {
		t.Fatal("duplicate app accepted")
	}
}

func TestAppBackupRejectsManagedOutputAndIncompleteCapture(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	if err := h.BackupApps(nil, BackupOptions{All: true, OutputDir: filepath.Join(h.Manager.StateDir, "backups")}); err == nil {
		t.Fatal("managed output accepted")
	}
	runner.failDump = true
	if err := h.BackupApps(nil, BackupOptions{All: true, OutputDir: filepath.Dir(output)}); err == nil {
		t.Fatal("failed capture reported success")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(output), "*.tar.gz"))
	if len(files) != 0 {
		t.Fatal("incomplete archive published")
	}
}

func TestAppBackupRejectsConflictingSharedFilesOnRestore(t *testing.T) {
	h, _, output, cleanup := fullFixture(t, false)
	defer cleanup()
	addSecondBackupApp(t, h)
	directory := filepath.Dir(output)
	if err := h.BackupApps([]string{"app"}, BackupOptions{OutputDir: directory}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.TemplatesDir, "php-cli.ini.tmpl"), []byte("; edited shared setting\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.BackupApps([]string{"second"}, BackupOptions{OutputDir: directory}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(directory, "*.tar.gz"))
	fresh := freshFullHost(t)
	fresh.DryRun = true
	if err := fresh.RestoreArchives(files, RestoreOptions{}); err == nil || !strings.Contains(fmt.Sprint(err), "conflicting shared file") {
		t.Fatal("conflicting shared settings accepted", err)
	}
}
