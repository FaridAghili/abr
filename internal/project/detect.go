// Package project inspects local checkouts without executing project code.
package project

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Detect returns an empty type for unknown or ambiguous projects, so the caller
// can ask the user. Root framework files take precedence over package metadata.
func Detect(directory string) (string, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	laravel, nuxt := false, false
	for _, name := range []string{"artisan", "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs", "nuxt.config.mts", "nuxt.config.cjs", "nuxt.config.cts"} {
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if name == "artisan" {
			laravel = true
		} else {
			nuxt = true
		}
	}
	if !laravel && !nuxt {
		for _, manifest := range []string{"composer.json", "package.json"} {
			info, err := root.Lstat(manifest)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Size() > 1<<20 {
				continue
			}
			payload, err := readManifest(root, manifest, info)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", manifest, err)
			}
			var data struct {
				Require         map[string]json.RawMessage `json:"require"`
				Dependencies    map[string]json.RawMessage `json:"dependencies"`
				DevDependencies map[string]json.RawMessage `json:"devDependencies"`
			}
			err = json.Unmarshal(payload, &data)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", manifest, err)
			}
			if manifest == "composer.json" {
				_, laravel = data.Require["laravel/framework"]
			} else {
				_, dependency := data.Dependencies["nuxt"]
				_, devDependency := data.DevDependencies["nuxt"]
				nuxt = dependency || devDependency
			}
		}
	}
	if laravel != nuxt {
		if laravel {
			return "laravel", nil
		}
		return "nuxt", nil
	}
	return "", nil
}

func readManifest(root *os.Root, name string, expected os.FileInfo) ([]byte, error) {
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("manifest must be a filename")
	}
	// A checkout can change while being inspected. Never follow a swapped
	// symlink or block opening a FIFO, even when Lstat saw a regular file.
	// Root.OpenFile follows in-root symlinks even with O_NOFOLLOW. Open the
	// basename relative to a pinned directory descriptor to enforce the flag.
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(expected, current) || current.Size() > 1<<20 {
		return nil, fmt.Errorf("manifest changed while detecting the framework; try again")
	}
	payload, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > 1<<20 {
		return nil, fmt.Errorf("manifest exceeds the 1 MiB limit")
	}
	return payload, nil
}
