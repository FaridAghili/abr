package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
)

func mergeFixture(t *testing.T, name string, port int) (Manifest, string) {
	t.Helper()
	c := config.Default()
	c.Apps = []config.App{{Name: name, User: "abr-" + name, Directory: "/srv/apps/" + name, Type: "nuxt", Domain: name + ".test"}}
	r := ports.Empty()
	r.Assignments = []ports.Assignment{{App: name, Purpose: "nuxt-http", Port: port}}
	m := Manifest{Version: 1, Scope: "app", CreatedAt: time.Now().UTC(), Config: c, Ports: r, Apps: []App{{Name: name, Repository: "git@github.com:fixture/" + name + ".git", Branch: "main", Commit: strings.Repeat("a", 40)}}, Settings: Settings{Hostname: "fixture-host", RoadRunnerVersion: "latest", NoRedis: true}, Files: map[string]File{}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m, t.TempDir()
}

func TestMergeRejectsPortConflictsBeforeCopying(t *testing.T) {
	first, source := mergeFixture(t, "first", 10001)
	second, incoming := mergeFixture(t, "second", 10001)
	target := t.TempDir()
	var combined Manifest
	if err := Merge(&combined, first, source, target); err != nil {
		t.Fatal(err)
	}
	if err := Merge(&combined, second, incoming, target); err == nil || !strings.Contains(err.Error(), "duplicate port") {
		t.Fatal("port conflict accepted", err)
	}
	if len(combined.Apps) != 1 {
		t.Fatal("failed merge changed target manifest")
	}
	if files, _ := os.ReadDir(target); len(files) != 0 {
		t.Fatal("failed merge copied files")
	}
}

func TestMergeAcceptsDistinctAppsAndRejectsMixedScopes(t *testing.T) {
	first, source := mergeFixture(t, "first", 10001)
	second, incoming := mergeFixture(t, "second", 10002)
	target := t.TempDir()
	var combined Manifest
	if err := Merge(&combined, first, source, target); err != nil {
		t.Fatal(err)
	}
	if err := Merge(&combined, second, incoming, target); err != nil {
		t.Fatal(err)
	}
	if len(combined.Apps) != 2 || len(combined.Ports.Assignments) != 2 {
		t.Fatal("lost app settings")
	}
	third, data := mergeFixture(t, "third", 10003)
	third.Scope = ""
	if err := Merge(&combined, third, data, filepath.Join(target, "mixed")); err == nil {
		t.Fatal("full-server archive mixed with app archives")
	}
}

func TestMergeMovesOnlyNewEntriesWithoutReplacing(t *testing.T) {
	first, source := mergeFixture(t, "first", 10001)
	second, incoming := mergeFixture(t, "second", 10002)
	write := func(directory, name, data string) File {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(data))
		return File{Size: int64(len(data)), Mode: 0600, SHA256: hex.EncodeToString(sum[:])}
	}
	first.Files["shared"] = File{Mode: 0700, Directory: true}
	first.Files["shared/Caddyfile"] = write(source, "shared/Caddyfile", "shared")
	second.Files["shared"] = File{Mode: 0700, Directory: true}
	second.Files["shared/Caddyfile"] = write(incoming, "shared/Caddyfile", "shared")
	second.Files["apps"] = File{Mode: 0700, Directory: true}
	second.Files["apps/second.env"] = write(incoming, "apps/second.env", "second secret")
	target := t.TempDir()
	var combined Manifest
	if err := Merge(&combined, first, source, target); err != nil {
		t.Fatal(err)
	}
	if err := Merge(&combined, second, incoming, target); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "apps/second.env")); err != nil || string(data) != "second secret" {
		t.Fatal("new entry not merged", err)
	}
	if _, err := os.Stat(filepath.Join(incoming, "apps/second.env")); !os.IsNotExist(err) {
		t.Fatal("new entry was copied instead of moved")
	}
	if _, err := os.Stat(filepath.Join(incoming, "shared/Caddyfile")); err != nil {
		t.Fatal("shared entry already in the target was moved", err)
	}
	// Scheduled app backups differ in time: the newest shared files and settings win.
	older, oldData := mergeFixture(t, "older", 10003)
	older.CreatedAt = combined.CreatedAt.Add(-time.Hour)
	older.Settings.Hostname = "old-host"
	older.Files["shared"] = File{Mode: 0700, Directory: true}
	older.Files["shared/Caddyfile"] = write(oldData, "shared/Caddyfile", "older shared")
	if err := Merge(&combined, older, oldData, target); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(target, "shared/Caddyfile")); string(contents) != "shared" || combined.Settings.Hostname != "fixture-host" {
		t.Fatal("older archive replaced newer shared state")
	}
	newest, newData := mergeFixture(t, "newest", 10004)
	newest.CreatedAt = combined.CreatedAt.Add(time.Hour)
	newest.Settings.Hostname = "new-host"
	newest.Files["shared"] = File{Mode: 0700, Directory: true}
	newest.Files["shared/Caddyfile"] = write(newData, "shared/Caddyfile", "newest shared")
	if err := Merge(&combined, newest, newData, target); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(target, "shared/Caddyfile")); string(contents) != "newest shared" || combined.Settings.Hostname != "new-host" || combined.Files["shared/Caddyfile"].Size != int64(len("newest shared")) {
		t.Fatal("newest shared state did not win")
	}
	if len(combined.Apps) != 4 {
		t.Fatal("lost apps", len(combined.Apps))
	}
	duplicate, duplicateData := mergeFixture(t, "second", 10005)
	if err := Merge(&combined, duplicate, duplicateData, target); err == nil {
		t.Fatal("same app accepted twice")
	}
}
