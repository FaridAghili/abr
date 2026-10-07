package host

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"abr/internal/config"
)

// Build-time Brotli keeps the official Caddy binary and avoids response-time CPU cost.
func (h Host) compressAssets(a config.App, environment map[string]string) error {
	suffix := "public/build/assets"
	if a.Type == "nuxt" {
		suffix = ".output/public/_nuxt"
	}
	directory := h.path(filepath.Join(a.Directory, suffix))
	if h.DryRun {
		h.say("Would generate Brotli sidecars for built assets under %s as %s", directory, a.User)
		return nil
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if resolved != directory {
		return fmt.Errorf("build assets directory must not contain symlinks")
	}
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".js", ".css", ".svg", ".json", ".wasm":
		default:
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 512 {
			return nil
		}
		if info, err := os.Lstat(path + ".br"); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("compressed asset must not be a symlink/nonregular file")
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		_, err = h.asUser(a, environment, false, "/usr/bin/brotli", "--force", "--quality=5", "--", path)
		return err
	})
}
