package host

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"abr/internal/backup"
	"abr/internal/config"
)

type backupCodeRunner struct {
	base   Runner
	output io.Writer
}

func (r backupCodeRunner) Run(c Command) ([]byte, error) {
	if c.Name == "runuser" && slices.Contains(c.Args, "git") && slices.Contains(c.Args, "bundle") && slices.Contains(c.Args, "create") {
		c.Stdout = r.output
	}
	return r.base.Run(c)
}

// A self-contained bundle retains the deployed branch and its lockfiles. It is
// parsed by the unprivileged clone account on restore, never by root.
func (h Host) captureBackupCode(a config.App, record backup.App, stage string) error {
	if _, err := os.Lstat(h.path(filepath.Join(a.Directory, ".gitmodules"))); err == nil {
		return fmt.Errorf("%s uses Git submodules; a standalone backup requires their source in the project repository", a.Name)
	} else if !os.IsNotExist(err) {
		return err
	}
	directory := filepath.Join(stage, "apps", a.Name)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(directory, "repository.bundle"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	base := h.Runner
	if base == nil {
		base = ExecRunner{Output: h.Output}
	}
	h.Runner = backupCodeRunner{base: base, output: f}
	if _, err := h.asUser(a, nil, true, "git", "bundle", "create", "-", "refs/heads/"+record.Branch); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := h.captureWorkingSource(a, directory); err != nil {
		return err
	}
	current, err := h.asUser(a, nil, true, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(current)) != record.Commit {
		return fmt.Errorf("%s code changed during backup; archive not published", a.Name)
	}
	return nil
}

func validSourcePath(name string) bool {
	return fs.ValidPath(name) && name != "." && !strings.ContainsAny(name, "\\\x00\r\n") && name != ".git" && !strings.HasPrefix(name, ".git/") && name != ".env"
}

func excludedSourcePath(name string, laravel bool) bool {
	if name == ".env" || name == ".git" || strings.HasPrefix(name, ".git/") {
		return true
	}
	if laravel {
		if name == "public/storage" {
			return true
		}
		for _, directory := range []string{"storage/app", "storage/logs", "storage/framework", "bootstrap/cache", "public/build", "vendor"} {
			if name == directory || strings.HasPrefix(name, directory+"/") {
				return true
			}
		}
	}
	for _, part := range strings.Split(name, "/") {
		if slices.Contains([]string{"node_modules", ".nuxt", ".output", ".cache"}, part) {
			return true
		}
	}
	return false
}

// Preserve working files, including ignored local configuration such as .npmrc.
// Omit known dependency/build/cache trees; app storage and .env are separate.
func (h Host) captureWorkingSource(a config.App, destination string) error {
	data, err := h.asUser(a, nil, true, "git", "ls-tree", "-r", "-z", "--name-only", "HEAD")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(h.path(a.Directory))
	if err != nil {
		return err
	}
	defer root.Close()
	var deleted []string
	seen := map[string]bool{}
	for _, name := range strings.Split(string(data), "\x00") {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if excludedSourcePath(name, a.Type == "laravel") {
			continue
		}
		if !validSourcePath(name) {
			return fmt.Errorf("unsafe source path in %s", a.Name)
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			deleted = append(deleted, name)
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source must contain regular files without symlinks: %s", name)
		}
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if excludedSourcePath(name, a.Type == "laravel") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !validSourcePath(name) {
			return fmt.Errorf("unsafe source path in %s", a.Name)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(destination, "source", filepath.FromSlash(name)), 0700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source must contain regular files without symlinks: %s", name)
		}
		return copyWorkingSourceFile(root, name, filepath.Join(destination, "source", filepath.FromSlash(name)), info.Mode())
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(destination, "source"), 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(deleted)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "source-deleted.json"), encoded, 0600)
}

func copyWorkingSourceFile(root *os.Root, name, destination string, mode os.FileMode) error {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source changed type during backup")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, f)
	if copyErr == nil && mode.Perm()&0111 != 0 {
		copyErr = out.Chmod(0700)
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (h Host) restoreWorkingSource(stage string, a config.App, m backup.Manifest) error {
	base := filepath.Join(stage, "apps", a.Name)
	data, err := readProjectFile(filepath.Join(base, "source-deleted.json"), 1<<20)
	if err != nil {
		return err
	}
	var deleted []string
	if err := json.Unmarshal(data, &deleted); err != nil {
		return fmt.Errorf("invalid deleted-source inventory")
	}
	root, err := os.OpenRoot(h.path(a.Directory))
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range deleted {
		if !validSourcePath(name) {
			return fmt.Errorf("unsafe deleted-source path")
		}
		parent, err := openRestoreDirectory(root, filepath.Dir(filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		err = parent.Remove(filepath.Base(filepath.FromSlash(name)))
		parent.Close()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := copyIntoRoot(filepath.Join(base, "source"), root); err != nil {
		return err
	}
	// Extracted files are always private. Restore only executable bits from the
	// checked inventory; never setuid or group/world write permissions.
	for name, record := range m.Files {
		prefix := "apps/" + a.Name + "/source/"
		if !record.Directory && strings.HasPrefix(name, prefix) && record.Mode&0111 != 0 {
			rel := strings.TrimPrefix(name, prefix)
			f, err := root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			err = f.Chmod(0700)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
