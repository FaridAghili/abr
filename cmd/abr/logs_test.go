package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsClearCLI(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps")}
	if _, err := invoke(t, append(paths, "register", "--config-only", "--name", "app", "--type", "laravel", "--domain", "example.com")...); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadDir(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"logs", "clear", "app", "--dry-run"},
		{"logs", "clear", "--all", "--type", "application", "--dry-run"},
		{"logs", "clear", "app", "--type=deployment", "--yes", "--dry-run"},
	} {
		out, err := invoke(t, append(paths, args...)...)
		if err != nil || !strings.Contains(out, "Would clear app") || strings.Contains(out, "app: cleared") {
			t.Fatalf("preview failed: %s %v", out, err)
		}
		if strings.Contains(strings.Join(args, " "), "application") && strings.Contains(out, "deployments/app") {
			t.Fatal("application selector included deployment logs")
		}
		if strings.Contains(strings.Join(args, " "), "deployment") && strings.Contains(out, "storage/logs") {
			t.Fatal("deployment selector included application logs")
		}
	}
	stateAfter, err := os.ReadDir(filepath.Join(dir, "state"))
	if err != nil || len(stateBefore) != len(stateAfter) {
		t.Fatal("preview changed state")
	}
	for _, args := range [][]string{
		{"logs", "clear"},
		{"logs", "clear", "app", "--all", "--yes", "--dry-run"},
		{"logs", "clear", "app"},
		{"logs", "clear", "app", "--type", "invalid", "--dry-run"},
		{"logs", "clear", "missing", "--dry-run"},
	} {
		if _, err := invoke(t, append(paths, args...)...); err == nil {
			t.Fatalf("invalid clear command accepted: %v", args)
		}
	}
	// Existing journal viewing remains available, including service selection.
	if out, err := invoke(t, append(paths, "logs", "app", "web", "--dry-run")...); err == nil || !strings.Contains(err.Error(), "no installed managed services") {
		t.Fatalf("journal command changed: %s %v", out, err)
	}
}
