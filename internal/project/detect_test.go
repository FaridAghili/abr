package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFramework(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"artisan", map[string]string{"artisan": "<?php"}, "laravel"},
		{"nuxt-ts", map[string]string{"nuxt.config.ts": "export default {}"}, "nuxt"},
		{"nuxt-js", map[string]string{"nuxt.config.js": "export default {}"}, "nuxt"},
		{"nuxt-esm", map[string]string{"nuxt.config.mjs": "export default {}"}, "nuxt"},
		{"composer", map[string]string{"composer.json": `{"require":{"laravel/framework":"^13"}}`}, "laravel"},
		{"nuxt-dev", map[string]string{"package.json": `{"devDependencies":{"nuxt":"^4"}}`}, "nuxt"},
		{"nuxt-package", map[string]string{"package.json": `{"dependencies":{"nuxt":"^4"}}`}, "nuxt"},
		{"ambiguous", map[string]string{"artisan": "", "nuxt.config.ts": ""}, ""},
		{"unrelated", map[string]string{"package.json": `{"dependencies":{"vue":"^3"}}`}, ""},
		{"unknown", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Detect(dir)
			if err != nil || got != tc.want {
				t.Fatalf("detected %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDetectDoesNotExecuteOrFollowFrameworkLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/missing/target", filepath.Join(dir, "artisan")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nuxt.config.ts"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := Detect(dir); got != "" || err != nil {
		t.Fatalf("non-files detected: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(dir); err == nil {
		t.Fatal("manifest read failure hidden")
	}
}
