// Package project inspects local checkouts without executing project code.
package project

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Detect returns an empty type for unknown or ambiguous projects, so the caller
// can ask the user. Root framework files take precedence over package metadata.
func Detect(directory string) (string, error) {
	laravel, nuxt := false, false
	for _, name := range []string{"artisan", "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs", "nuxt.config.mts", "nuxt.config.cjs", "nuxt.config.cts"} {
		info, err := os.Lstat(filepath.Join(directory, name))
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
			path := filepath.Join(directory, manifest)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Size() > 1<<20 {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			var data struct {
				Require         map[string]json.RawMessage `json:"require"`
				Dependencies    map[string]json.RawMessage `json:"dependencies"`
				DevDependencies map[string]json.RawMessage `json:"devDependencies"`
			}
			err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&data)
			f.Close()
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
