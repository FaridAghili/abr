// Package host contains Linux host operations. Portable packages never call it.
package host

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"sites-manager/internal/manager"
	"sites-manager/internal/storage"
)

type Command struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string
	Input   []byte
	Private bool // Never display SQL or its error output, which can contain credentials.
}

type Runner interface{ Run(Command) ([]byte, error) }

type ExecRunner struct{ Output io.Writer }

func (r ExecRunner) Run(c Command) ([]byte, error) {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = bytes.NewReader(c.Input)
	var output bytes.Buffer
	var w io.Writer = &output
	if !c.Private && r.Output != nil {
		w = io.MultiWriter(w, r.Output)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		// Output is streamed/logged for ordinary commands; never put it in errors.
		return output.Bytes(), fmt.Errorf("%s failed: %w", c.Name, err)
	}
	return output.Bytes(), nil
}

type Host struct {
	Manager      manager.Manager
	TemplatesDir string
	AppsDir      string
	DryRun       bool
	Output       io.Writer
	Runner       Runner
	// Only tests override these; CLI exposes no guard bypass or alternate host root.
	check func() error
	root  string
}

func Require() error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("host operations require Ubuntu 26.04 Linux AMD64; use --dry-run to preview on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("host operations require root; use sudo")
	}
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = strings.Trim(v, `"`)
		}
	}
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "26.04" {
		return fmt.Errorf("host operations support Ubuntu 26.04 only")
	}
	return nil
}

func (h Host) guard() error {
	if h.DryRun {
		return nil
	}
	if h.check != nil {
		return h.check()
	}
	return Require()
}

func (h Host) locked(fn func() error) error {
	if err := h.guard(); err != nil {
		return err
	}
	if h.DryRun {
		return fn()
	}
	return storage.WithLock(filepath.Join(h.Manager.StateDir, "host.lock"), fn)
}

func (h Host) path(path string) string {
	if h.root == "" {
		return path
	}
	if path == h.root || strings.HasPrefix(path, h.root+string(filepath.Separator)) {
		return path
	}
	return filepath.Join(h.root, path)
}

func (h Host) say(format string, args ...any) {
	if h.Output != nil {
		fmt.Fprintf(h.Output, format+"\n", args...)
	}
}

func (h Host) run(label string, c Command) ([]byte, error) {
	if h.DryRun {
		h.say("Would %s", label)
		return nil, nil
	}
	h.say("%s", label)
	r := h.Runner
	if r == nil {
		r = ExecRunner{h.Output}
	}
	return r.Run(c)
}

func (h Host) command(name string, args ...string) error {
	_, err := h.run(strings.Join(append([]string{name}, args...), " "), Command{Name: name, Args: args})
	return err
}

func (h Host) write(path string, data []byte, mode os.FileMode) error {
	if h.DryRun {
		h.say("Would write %s (mode %04o)", path, mode)
		return nil
	}
	if mode != 0600 {
		if err := os.MkdirAll(filepath.Dir(h.path(path)), 0755); err != nil {
			return err
		}
	}
	return storage.AtomicWriteMode(h.path(path), data, mode)
}

func (h Host) read(path string) ([]byte, error) { return os.ReadFile(h.path(path)) }

func (h Host) removeFile(path string) error {
	if h.DryRun {
		h.say("Would remove managed file %s", path)
		return nil
	}
	err := os.Remove(h.path(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
