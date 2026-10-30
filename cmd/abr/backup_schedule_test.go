package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupScheduleForManyAppsPreview(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	paths := []string{"--config", config, "--state-dir", filepath.Join(dir, "state")}
	for _, name := range []string{"blog", "shop"} {
		if _, err := invoke(t, append(paths, "register", "--config-only", "--name", name, "--type", "nuxt", "--domain", name+".test")...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"backup", "schedule", "--schedule", "daily"},
		{"backup", "schedule", "blog", "--all", "--schedule", "daily"},
		{"backup", "schedule", "blog"},
		{"backup", "schedule", "blog", "--schedule", "25h"},
		{"backup", "schedule", "blog", "--schedule", "daily", "--keep", "1001"},
		{"backup", "blog", "--local", "--transfer"},
	} {
		if _, err := invoke(t, append(append(paths, "--dry-run"), args...)...); err == nil {
			t.Fatal("invalid backup arguments accepted", args)
		}
	}
	before, _ := os.ReadFile(config)
	out, err := invoke(t, append(paths, "--dry-run", "backup", "schedule", "--all", "--schedule", "4h", "--keep", "30")...)
	if err != nil || !strings.Contains(out, "Would set every 4h, keep newest 30 for 2 apps: blog, shop") {
		t.Fatalf("preview inaccurate: %s %v", out, err)
	}
	if after, _ := os.ReadFile(config); string(after) != string(before) {
		t.Fatal("preview changed the configuration")
	}
}
