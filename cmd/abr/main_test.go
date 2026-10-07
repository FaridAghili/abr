package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func invoke(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(args, &out, &stderr)
	return out.String(), err
}

func TestCommands(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	register := append(append([]string{}, paths...), "register", "--config-only", "--name", "demo", "--dir", "/srv/demo", "--user", "demo", "--type", "laravel", "--domain", "demo.test", "--queue-workers", "2", "--scheduler")
	out, err := invoke(t, register...)
	if err != nil || !strings.Contains(out, "Registered demo") {
		t.Fatalf("%s %v", out, err)
	}
	for _, cmd := range [][]string{{"config", "validate"}, {"list"}, {"ports"}, {"doctor"}} {
		args := append(append([]string{}, paths...), cmd...)
		if _, err := invoke(t, args...); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
	}
	state, err := os.ReadFile(filepath.Join(dir, "state", "ports.json"))
	if err != nil || !strings.Contains(string(state), `"assignments": []`) {
		t.Fatalf("FPM allocated ports: %s %v", state, err)
	}
	if out, err := invoke(t, "version"); err != nil || !strings.HasPrefix(out, "abr 0.1.0 (") {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := invoke(t, register...); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	// Path flags may also follow the command.
	args := append([]string{"list"}, paths...)
	if out, err := invoke(t, args...); err != nil || !strings.Contains(out, "demo") {
		t.Fatalf("%s %v", out, err)
	}
	for _, cmd := range []string{"deploy", "enable", "remove", "unknown"} {
		if out, err := invoke(t, cmd); err == nil || out != "" {
			t.Fatalf("%s reported success: %s %v", cmd, out, err)
		}
	}
	for _, args := range [][]string{{"register", "--help"}, {"--help"}, {"ports", "--help"}} {
		if _, err := invoke(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := invoke(t, "register"); err == nil {
		t.Fatal("accepted missing required flags")
	}
	if _, err := invoke(t, "list", "extra"); err == nil {
		t.Fatal("ignored extra argument")
	}
}

func TestOccupiedImportDoesNotRegister(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	args := []string{"register", "--config-only", "--config", path, "--state-dir", filepath.Join(dir, "state"), "--name", "octane", "--dir", "/srv/octane", "--user", "octane", "--type", "laravel", "--domain", "octane.test", "--web-driver", "octane", "--port", "octane-http=" + port}
	out, err := invoke(t, args...)
	if err == nil || out != "" || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed registration saved config: %v", err)
	}
}

func TestInteractiveCommandNeedsTerminal(t *testing.T) {
	if out, err := invoke(t, "tui"); err == nil || out != "" || !strings.Contains(err.Error(), "requires terminal") {
		t.Fatalf("%s %v", out, err)
	}
	if out, err := invoke(t); err != nil || !strings.Contains(out, "Usage:") {
		t.Fatalf("no-argument noninteractive invocation: %s %v", out, err)
	}
	if _, err := invoke(t, "tui", "extra"); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("unexpected args accepted: %v", err)
	}
}

func TestSharedGitCommandsAndPortablePreviews(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	apps := filepath.Join(dir, "apps")
	paths := []string{"--state-dir", state, "--apps-dir", apps, "--dry-run"}
	for _, command := range [][]string{{"git", "setup"}, {"git", "setup", "--key", "/nonexistent/key"}, {"clone", "git@github.com:owner/repo.git", filepath.Join(apps, "repo")}} {
		args := append(append([]string{}, paths...), command...)
		out, err := invoke(t, args...)
		if err != nil || !strings.Contains(out, "Would") {
			t.Fatalf("preview %v: %s %v", command, out, err)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("Git preview wrote state")
	}
	for _, command := range [][]string{{"git"}, {"git", "unknown"}, {"git", "setup", "extra"}, {"clone"}, {"clone", "one"}, {"clone", "one", "two", "three"}} {
		if _, err := invoke(t, command...); err == nil {
			t.Fatalf("invalid command succeeded: %v", command)
		}
	}
}

func TestDatabaseTransferCommandValidationAndPreview(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	args := append(append([]string{}, paths...), "register", "--config-only", "--name", "demo", "--dir", "/srv/apps/demo", "--type", "laravel", "--domain", "demo.test")
	if _, err := invoke(t, args...); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"database", "backup", "demo", "--output-dir", filepath.Join(dir, "backups"), "--dry-run"},
		{"database", "backup", "--all", "--output-dir", filepath.Join(dir, "backups"), "--dry-run"},
		{"database", "import", "demo", "/tmp/backup.sql", "--dry-run"},
	} {
		out, err := invoke(t, append(append([]string{}, paths...), command...)...)
		if err != nil || !strings.Contains(out, "Would") {
			t.Fatal(out, err)
		}
	}
	for _, command := range [][]string{
		{"database", "backup"}, {"database", "backup", "demo"},
		{"database", "backup", "demo", "--all", "--output-dir", "/tmp/backups"},
		{"database", "import"}, {"database", "import", "demo", "/tmp/backup.sql"},
	} {
		if out, err := invoke(t, append(append([]string{}, paths...), command...)...); err == nil || out != "" {
			t.Fatal("invalid transfer reported success", out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Fatal("preview wrote backup files")
	}
}
