package host

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"abr/internal/config"
)

// Brotli sidecars built after each deployment keep the official Caddy binary
// and avoid response-time CPU cost. Build directories are emptied by each
// build, so no stale sidecar outlives its asset.
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
	// Bound argv size and avoid one runuser/setpriv process per asset. Each
	// batch still executes Brotli exclusively with the application's privileges.
	batch := make([]string, 0, 64)
	bytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// The app already serves while this runs; Caddy serves assets without a
		// sidecar uncompressed or gzip-encoded until it appears. Best quality
		// costs nothing at request time, and low priority leaves CPU to the app.
		args := append([]string{"-n", "10", "/usr/bin/brotli", "--force", "--quality=11", "--lgwin=24", "--"}, batch...)
		_, err := h.asUser(a, environment, false, "nice", args...)
		batch = batch[:0]
		bytes = 0
		return err
	}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
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
		if len(batch) == cap(batch) || bytes+len(path)+1 > 32<<10 {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, path)
		bytes += len(path) + 1
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}
