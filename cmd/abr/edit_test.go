package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/config"
)

func TestEditChangesOnlySuppliedFlagsAndPreviewDoesNotSave(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	if _, err := invoke(t, append(paths, "register", "--name", "app", "--type", "laravel", "--domain", "example.com", "--alias", "old.example.com", "--scheduler", "--queue-workers", "2", "--config-only")...); err != nil {
		t.Fatal(err)
	}
	out, err := invoke(t, append(paths, "edit", "app", "--scheduler=false", "--alias", "", "--config-only")...)
	if err != nil || !strings.Contains(out, "deploy") {
		t.Fatalf("edit guidance: %q %v", out, err)
	}
	c, err := config.Load(paths[1])
	if err != nil || c.Apps[0].Scheduler.Enabled || c.Apps[0].Queue.Workers != 2 || len(c.Apps[0].Aliases) != 0 || !c.Apps[0].Database.Enabled {
		t.Fatalf("omitted or empty flags mishandled: %+v %v", c, err)
	}
	before, _ := os.ReadFile(paths[1])
	if _, err := invoke(t, append(paths, "edit", "app", "--domain", "changed.example.com", "--config-only", "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(paths[1])
	if string(before) != string(after) {
		t.Fatal("preview saved")
	}
	if _, err := invoke(t, append(paths, "edit", "app", "--config-only")...); err == nil {
		t.Fatal("accepted empty edit")
	}
}
