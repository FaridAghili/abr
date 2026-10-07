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
	"sync"

	"abr/internal/manager"
	"abr/internal/storage"
)

type Command struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string
	Input   []byte
	Stdin   io.Reader // Stream SQL imports without retaining them in memory.
	Stdout  io.Writer // Stream dumps directly to a private file, never logs.
	Stream  bool      // Commands whose return output is unused must not accumulate it.
	Private bool      // Never display SQL or its error output, which can contain credentials.
}

type Runner interface{ Run(Command) ([]byte, error) }

type ExecRunner struct{ Output io.Writer }

const maxCommandOutput = 1 << 20
const hostPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type commandOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func (w *commandOutput) Bytes() []byte { return w.buffer.Bytes() }

func (w *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxCommandOutput - w.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	_, _ = w.buffer.Write(p)
	return n, nil
}

func (r ExecRunner) Run(c Command) ([]byte, error) {
	// Resolve before exec using a fixed path; exec.Command otherwise searches the
	// caller's PATH even when cmd.Env has a safe replacement.
	name := c.Name
	if !filepath.IsAbs(name) {
		if strings.ContainsRune(name, '/') {
			return nil, fmt.Errorf("command must be an absolute path or a bare name")
		}
		name = ""
		for _, dir := range filepath.SplitList(hostPath) {
			candidate := filepath.Join(dir, c.Name)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				name = candidate
				break
			}
		}
		if name == "" {
			return nil, fmt.Errorf("command %s not found in the host path", c.Name)
		}
	}
	cmd := exec.Command(name, c.Args...)
	cmd.Dir = c.Dir
	// Do not expose sudo's environment or honor loader, Git, npm, PHP, or proxy
	// overrides while executing privileged host operations.
	cmd.Env = append([]string{"PATH=" + hostPath, "HOME=/root", "USER=root", "LOGNAME=root", "LANG=C.UTF-8"}, c.Env...)
	cmd.Stdin = bytes.NewReader(c.Input)
	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	}
	var output commandOutput
	var w io.Writer = &output
	var log io.Writer
	if !c.Private && r.Output != nil {
		// os/exec copies stdout and stderr concurrently. Callers include buffers
		// and TUI writers that do not support concurrent writes.
		log = &synchronizedWriter{w: r.Output}
		w = io.MultiWriter(w, log)
	}
	cmd.Stdout = w
	if c.Stream {
		cmd.Stdout = io.Discard
		if log != nil {
			cmd.Stdout = log
		}
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	cmd.Stderr = io.Discard
	if log != nil {
		cmd.Stderr = log
	}
	if err := cmd.Run(); err != nil {
		// Output is streamed/logged for ordinary commands; never put it in errors.
		return output.Bytes(), fmt.Errorf("%s failed: %w", c.Name, err)
	}
	if output.truncated {
		return nil, fmt.Errorf("%s exceeded the command output capture limit", c.Name)
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
	if err := Require(); err != nil {
		return err
	}
	// Root-managed state/configuration must never live beneath directories an
	// application (or another unprivileged user) can replace.
	for _, dir := range []string{h.Manager.StateDir, filepath.Dir(h.Manager.ConfigPath), h.TemplatesDir, h.AppsDir} {
		if err := h.trustedAncestor(dir); err != nil {
			return err
		}
	}
	for _, path := range []string{h.Manager.ConfigPath, h.Manager.ConfigPath + ".lock", h.Manager.RegistryPath(), filepath.Join(h.Manager.StateDir, "ports.lock"), filepath.Join(h.Manager.StateDir, "host.lock")} {
		if err := h.trustedFile(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	entries, err := os.ReadDir(h.TemplatesDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmpl") {
			if err := h.trustedFile(filepath.Join(h.TemplatesDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Host) locked(fn func() error) error {
	if err := h.guard(); err != nil {
		return err
	}
	if h.DryRun {
		return fn()
	}
	if err := h.trustedAncestor(h.path(h.Manager.StateDir)); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(h.Manager.StateDir), 0700); err != nil {
		return err
	}
	if err := h.trustedDirectory(h.path(h.Manager.StateDir)); err != nil {
		return err
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
	_, err := h.run(strings.Join(append([]string{name}, args...), " "), Command{Name: name, Args: args, Stream: true})
	return err
}

func (h Host) write(path string, data []byte, mode os.FileMode) error {
	if h.DryRun {
		h.say("Would write %s (mode %04o)", path, mode)
		return nil
	}
	dirMode := os.FileMode(0700)
	if mode != 0600 {
		dirMode = 0755
	}
	if err := h.trustedAncestor(filepath.Dir(h.path(path))); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path(path)), dirMode); err != nil {
		return err
	}
	if err := h.trustedDirectory(filepath.Dir(h.path(path))); err != nil {
		return err
	}
	return storage.AtomicWriteMode(h.path(path), data, mode)
}

func (h Host) read(path string) ([]byte, error) {
	if path == h.Manager.StateDir || strings.HasPrefix(path, h.Manager.StateDir+string(filepath.Separator)) {
		if err := h.trustedDirectory(filepath.Dir(h.path(path))); err != nil {
			return nil, err
		}
		if err := h.trustedFile(h.path(path)); err != nil {
			return nil, err
		}
	}
	return os.ReadFile(h.path(path))
}

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
