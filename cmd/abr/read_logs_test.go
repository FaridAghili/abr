package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLogsCLIOptionsAndAllAppPreview(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	state := filepath.Join(dir, "state")
	paths := []string{"--config", configPath, "--state-dir", state, "--apps-dir", filepath.Join(dir, "apps")}
	for _, name := range []string{"api", "web", "clear"} {
		if _, err := invoke(t, append(paths, "register", "--config-only", "--name", name, "--type", "laravel", "--domain", name+".example.com")...); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := invoke(t, append(paths, "logs", "--type", "application", "--dry-run", "--", "clear")...); err != nil || !strings.Contains(out, "Would read clear") {
		t.Fatal("an app named clear cannot read logs using the argument delimiter")
	}
	for _, kind := range []string{"application", "deployment"} {
		out, err := invoke(t, append(paths, "logs", "--all", "--type", kind, "--lines", "20", "--dry-run")...)
		if err != nil || !strings.Contains(out, "Would read api") || !strings.Contains(out, "Would read web") || !strings.Contains(out, "last 20 lines") {
			t.Fatalf("all-app preview failed: %s %v", out, err)
		}
		out, err = invoke(t, append(paths, "logs", "api", "--type="+kind, "--dry-run")...)
		if err != nil || strings.Contains(out, "Would read web") {
			t.Fatal("single-app preview included another app")
		}
	}
	if _, err := invoke(t, append(paths, "logs", "api", "web", "--follow", "--lines=20", "--dry-run")...); err == nil || !strings.Contains(err.Error(), "no installed managed services") {
		t.Fatal("journal flags were rejected or missing services were reported as installed")
	}
	for _, args := range [][]string{
		{"logs", "--all", "api"},
		{"logs", "api", "--type", "unknown"},
		{"logs", "api", "web", "--type", "application"},
		{"logs", "--all", "--type", "application", "--follow"},
		{"logs", "api", "--lines", "0"},
		{"logs", "api", "--lines", "1001"},
	} {
		if _, err := invoke(t, append(paths, append(args, "--dry-run")...)...); err == nil {
			t.Fatalf("invalid log options accepted: %v", args)
		}
	}
}
