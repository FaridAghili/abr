package backup

import (
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
