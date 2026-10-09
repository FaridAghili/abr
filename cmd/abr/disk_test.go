package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskCLISelectionFlagsAndPreview(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps")}
	for _, name := range []string{"api", "web"} {
		if _, err := invoke(t, append(paths, "register", "--config-only", "--name", name, "--type", "laravel", "--domain", name+".example.com")...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"disk", "--dry-run"},
		{"disk", "--all", "--refresh", "--dry-run"},
		{"disk", "api", "--refresh", "--dry-run"},
		{"disk", "--filesystem-only", "--dry-run"},
	} {
		out, err := invoke(t, append(paths, args...)...)
		if err != nil || !strings.Contains(out, "Would read live filesystem") {
			t.Fatalf("disk preview rejected: %v %s %v", args, out, err)
		}
		if strings.Contains(strings.Join(args, " "), "api") && (!strings.Contains(out, "Would measure api") || strings.Contains(out, "Would measure web")) {
			t.Fatal("single-app request included another app")
		}
		if strings.Contains(strings.Join(args, " "), "--all") && (!strings.Contains(out, "Would measure api") || !strings.Contains(out, "Would measure web")) {
			t.Fatal("all-app request missed an app")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state/disk-usage")); !os.IsNotExist(err) {
		t.Fatal("preview wrote disk cache state")
	}
	for _, args := range [][]string{
		{"disk", "api", "web", "--dry-run"},
		{"disk", "api", "--all", "--dry-run"},
		{"disk", "api", "--filesystem-only", "--dry-run"},
		{"disk", "missing", "--dry-run"},
		{"disk", "--json", "--dry-run"},
	} {
		if _, err := invoke(t, append(paths, args...)...); err == nil {
			t.Fatalf("invalid disk command accepted: %v", args)
		}
	}
}
