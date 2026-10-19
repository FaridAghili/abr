package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStatsCLIPreviewAndArguments(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps")}
	if _, err := invoke(t, append(paths, "register", "--config-only", "--name", "api", "--type", "laravel", "--domain", "api.example.com")...); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"stats", "--dry-run"}, {"stats", "api", "--interval", "2s", "--dry-run"}} {
		out, err := invoke(t, append(paths, args...)...)
		if err != nil || !strings.Contains(out, "Would read /proc") {
			t.Fatalf("stats preview rejected: %v %s %v", args, out, err)
		}
	}
	for _, args := range [][]string{
		{"stats", "api", "web", "--dry-run"},
		{"stats", "missing", "--dry-run"},
		{"stats", "--interval", "10ms", "--dry-run"},
		{"stats", "--interval", "2m", "--dry-run"},
	} {
		if _, err := invoke(t, append(paths, args...)...); err == nil {
			t.Fatalf("invalid stats command accepted: %v", args)
		}
	}
}
