package project

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestDetectRejectsTrailingManifestData(t *testing.T) {
	for _, suffix := range []string{` {}`, ` trailing`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"nuxt":"^4"}}`+suffix), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Detect(dir); err == nil {
			t.Fatal("accepted trailing manifest data")
		}
	}
}

func TestDetectSkipsUnsafeAndOversizedManifests(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "large"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "package.json")
			var err error
			switch kind {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "package.json")
				if err := os.WriteFile(outside, []byte(`{"dependencies":{"nuxt":"^4"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(outside, name)
			case "fifo":
				err = syscall.Mkfifo(name, 0600)
			case "directory":
				err = os.Mkdir(name, 0700)
			case "large":
				err = os.WriteFile(name, []byte(`{"dependencies":{"nuxt":"^4"}}`+strings.Repeat(" ", 1<<20)), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := Detect(dir); got != "" || err != nil {
				t.Fatalf("detected unsafe manifest: %q, %v", got, err)
			}
		})
	}
}

func TestManifestReplacementDuringDetection(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "file", "grown"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "package.json")
			if err := os.WriteFile(name, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			info, err := root.Lstat("package.json")
			if err != nil {
				t.Fatal(err)
			}
			if kind != "grown" {
				// Keep the original inode alive so the replacement cannot reuse it.
				if err := os.Rename(name, name+".original"); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "symlink":
				err = os.Symlink("package.json.original", name)
			case "fifo":
				err = syscall.Mkfifo(name, 0600)
			case "file":
				err = os.WriteFile(name, []byte(`{"dependencies":{"nuxt":"^4"}}`), 0600)
			case "grown":
				err = os.WriteFile(name, []byte(strings.Repeat(" ", (1<<20)+1)), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := readManifest(root, "package.json", info); err == nil || len(data) != 0 {
				t.Fatal("read a changed manifest")
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
