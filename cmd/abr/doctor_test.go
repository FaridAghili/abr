package main

import (
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/ports"
)

func TestDoctorPreviewAndInvalidReservations(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	if _, err := invoke(t, append(paths, "register", "--config-only", "--name", "demo", "--type", "nuxt", "--domain", "demo.test")...); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, append(paths, "doctor", "--dry-run")...); err != nil || !strings.Contains(out, "runtime health was not checked") {
		t.Fatalf("misleading preview: %s %v", out, err)
	}
	if err := ports.Empty().Save(filepath.Join(dir, "state", "ports.json")); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, append(paths, "doctor", "--dry-run")...); err == nil || !strings.Contains(out, "ERROR") || !strings.Contains(out, "no reservation") {
		t.Fatalf("invalid reservations accepted: %s %v", out, err)
	}
}
