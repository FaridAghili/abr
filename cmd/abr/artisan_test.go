package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestArtisanCLIFlagsPassThroughAndNuxtIsRejected(t *testing.T) {
	dir := t.TempDir()
	base := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps"), "--templates-dir", "../../templates"}
	for _, kind := range []string{"laravel", "nuxt"} {
		if _, err := invoke(t, append(slices.Clone(base), "register", "--name", kind, "--type", kind, "--domain", kind+".example.com", "--config-only")...); err != nil {
			t.Fatal(err)
		}
	}
	args := append(slices.Clone(base), "artisan", "--dry-run", "laravel", "db:seed", "--force", "--no-interaction", "--config=artisan-option", "--help")
	if out, err := invoke(t, args...); err != nil || !strings.Contains(out, "no command executed") {
		t.Fatalf("Artisan flags mishandled: %s %v", out, err)
	}
	if _, err := invoke(t, append(slices.Clone(base), "artisan", "--dry-run", "nuxt", "cache:clear")...); err == nil || !strings.Contains(err.Error(), "only available for Laravel") {
		t.Fatal("Nuxt accepted", err)
	}
}
