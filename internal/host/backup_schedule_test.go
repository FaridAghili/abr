package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"abr/internal/config"
	"abr/internal/services"
)

func setBackupSchedule(t *testing.T, h Host, name, schedule string, keep int) {
	t.Helper()
	changes := config.AppChanges{Values: config.App{Backup: config.Backup{Schedule: schedule, Keep: keep}}, Fields: []string{"backup-schedule", "backup-keep"}}
	if err := h.Manager.Edit(name, changes, true); err != nil {
		t.Fatal(err)
	}
}

func setLastBackup(t *testing.T, h Host, name string, when time.Time) {
	t.Helper()
	data, err := json.Marshal(backupRecord{LastSuccess: when, LastAttempt: when})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.write(h.backupRecordPath(name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledBackupsDueRetentionAndFailures(t *testing.T) {
	h, runner, output, cleanup := fullFixture(t, false)
	defer cleanup()
	addSecondBackupApp(t, h)
	setBackupSchedule(t, h, "app", "6h", 1)
	setBackupSchedule(t, h, "second", "daily", 0)
	directory := filepath.Dir(output)
	due := BackupOptions{Due: true, OutputDir: directory}
	if err := h.BackupApps(nil, due); err != nil {
		t.Fatal(err)
	}
	archives := func(app string) []string {
		files, _ := filepath.Glob(filepath.Join(directory, app+"-*.tar.gz"))
		return files
	}
	first := archives("app")
	if len(first) != 1 || len(archives("second")) != 1 {
		t.Fatal("first scheduled run did not back up every app")
	}
	buffer := h.Output.(*bytes.Buffer)
	buffer.Reset()
	if err := h.BackupApps(nil, due); err != nil || !strings.Contains(buffer.String(), "No scheduled app backups are due") {
		t.Fatal("backed up apps before their schedule was due", err, buffer.String())
	}
	// Within the slack before six hours, only the 6h app is due; keep 1 deletes its older archive.
	setLastBackup(t, h, "app", time.Now().Add(-6*time.Hour+5*time.Minute))
	if err := h.BackupApps(nil, due); err != nil {
		t.Fatal(err)
	}
	if latest := archives("app"); len(latest) != 1 || latest[0] == first[0] {
		t.Fatal("retention did not keep only the newest archive", latest)
	}
	if len(archives("second")) != 1 {
		t.Fatal("daily app backed up early")
	}
	// A failed app does not stop the others; it stays due and is reported.
	setLastBackup(t, h, "app", time.Now().Add(-7*time.Hour))
	setLastBackup(t, h, "second", time.Now().Add(-25*time.Hour))
	secondDirectory := filepath.Join(h.AppsDir, "second")
	runner.onCommand = func(c Command) error {
		if c.Name == "runuser" && c.Dir == secondDirectory && slices.Contains(c.Args, "bundle") {
			return errors.New("fixture capture failure")
		}
		return nil
	}
	before := time.Now()
	if err := h.BackupApps(nil, due); err == nil || !strings.Contains(err.Error(), "backup second") {
		t.Fatal("failed scheduled backup not reported", err)
	}
	appRecord, err := h.loadBackupRecord("app")
	if err != nil || appRecord.LastSuccess.Before(before.Add(-time.Second)) {
		t.Fatal("a failed app stopped the other scheduled backups", err)
	}
	record, err := h.loadBackupRecord("second")
	if err != nil || record.LastError == "" || !backupDue(config.App{Backup: config.Backup{Schedule: "daily"}}, record, time.Now()) {
		t.Fatal("failed app not recorded as still due", record, err)
	}
	buffer.Reset()
	if err := h.BackupStatus(); err != nil {
		t.Fatal(err)
	}
	status := buffer.String()
	if !regexp.MustCompile(`(?m)^app +every 6h +newest 1 +\d{4}-`).MatchString(status) || !regexp.MustCompile(`(?m)^second +daily +all +.* failed$`).MatchString(status) || !strings.Contains(status, "second: last attempt failed") {
		t.Fatal("backup status inaccurate", status)
	}
	if err := (BackupOptions{Due: true, All: true}).Validate(nil); err == nil {
		t.Fatal("--due combined with --all")
	}
}

func TestBackupScheduleEditInstallsManagedTimer(t *testing.T) {
	h, runner, _, cleanup := fullFixture(t, false)
	defer cleanup()
	changes := config.AppChanges{Values: config.App{Backup: config.Backup{Schedule: "daily", Keep: 7}}, Fields: []string{"backup-schedule", "backup-keep"}}
	if err := h.Edit("app", changes); err != nil {
		t.Fatal(err)
	}
	service, err := os.ReadFile(h.path("/etc/systemd/system/abr-backup.service"))
	if err != nil || !bytes.HasPrefix(service, []byte(services.Marker)) || !bytes.Contains(service, []byte(`--config "`+h.Manager.ConfigPath+`"`)) || !bytes.Contains(service, []byte(" backup --due\n")) {
		t.Fatal("backup service not rendered", string(service), err)
	}
	if timer, err := os.ReadFile(h.path("/etc/systemd/system/abr-backup.timer")); err != nil || !bytes.Contains(timer, []byte("OnCalendar=hourly")) {
		t.Fatal("backup timer not installed", err)
	}
	enabled := func() bool {
		for _, c := range runner.base.calls {
			if c.Name == "systemctl" && strings.Join(c.Args, " ") == "enable --now abr-backup.timer" {
				return true
			}
		}
		return false
	}
	if !enabled() {
		t.Fatal("backup timer not enabled")
	}
	runner.base.calls = nil
	if err := h.Edit("app", changes); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.base.calls {
		if c.Name == "systemctl" && len(c.Args) > 0 && c.Args[0] == "daemon-reload" {
			t.Fatal("unchanged timer reloaded systemd")
		}
	}
	if err := os.WriteFile(h.path("/etc/systemd/system/abr-backup.timer"), []byte("[Timer]\nOnCalendar=daily\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.Edit("app", changes); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatal("replaced an unmanaged timer", err)
	}
}

// The receiver needs GNU realpath/stat/sha256sum, as on the Linux backup server.
func TestBackupReceiverPublishesAndPrunesOnlyThisApp(t *testing.T) {
	for _, check := range [][]string{{"realpath", "-e", "/"}, {"stat", "-c", "%u", "/"}, {"sha256sum", "--version"}, {"sync", "--version"}} {
		if err := exec.Command(check[0], check[1:]...).Run(); err != nil {
			t.Skip("GNU coreutils required:", check[0])
		}
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The receiver requires a private directory owned by the SSH account.
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api-20261001000000.tar.gz", "api-20261002000000.tar.gz", "api-20261003000000.tar.gz", "api-v2-20261001000000.tar.gz", "notes.txt", ".abr-upload-stale", ".abr-upload-active"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(directory, ".abr-upload-stale"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(directory, "api-20260101000000.tar.gz")); err != nil {
		t.Fatal(err)
	}
	data := []byte("new archive")
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	name := "api-20261004000000.tar.gz"
	command := exec.Command("sh", "-c", backupReceiver, "abr-backup", directory, name, checksum, "2", "api")
	command.Stdin = bytes.NewReader(data)
	out, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ABR_BACKUP_OK "+checksum+" "+name+" 2" {
		t.Fatal("receiver failed", string(out), err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{".abr-upload-active", "api-20260101000000.tar.gz", "api-20261003000000.tar.gz", "api-20261004000000.tar.gz", "api-v2-20261001000000.tar.gz", "notes.txt"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatal("receiver retention removed the wrong files", names)
	}
	if published, _ := os.ReadFile(filepath.Join(directory, name)); !bytes.Equal(published, data) {
		t.Fatal("published archive differs")
	}
	command = exec.Command("sh", "-c", backupReceiver, "abr-backup", directory, "api-20261005000000.tar.gz", strings.Repeat("0", 64), "1", "api")
	command.Stdin = bytes.NewReader(data)
	if out, err := command.CombinedOutput(); err == nil || strings.Contains(string(out), "ABR_BACKUP_OK") {
		t.Fatal("corrupt upload accepted or pruned", string(out))
	}
	if _, err := os.Stat(filepath.Join(directory, "api-20261003000000.tar.gz")); err != nil {
		t.Fatal("failed upload pruned an existing backup", err)
	}
}
