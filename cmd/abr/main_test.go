package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"abr"
	"abr/internal/config"
)

func invoke(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(args, &out, &stderr)
	return out.String(), err
}

func TestBundledExampleDoesNotReadOrWriteHostConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "missing", "config.toml")
	state := filepath.Join(dir, "state")
	out, err := invoke(t, "config", "example", "--config", path, "--state-dir", state)
	if err != nil {
		t.Fatal(err)
	}
	want, err := abr.ExampleConfig()
	if err != nil || out != string(want) {
		t.Fatalf("example output: %v", err)
	}
	if _, err := config.Parse([]byte(out)); err != nil {
		t.Fatalf("invalid example: %v", err)
	}
	for _, target := range []string{filepath.Dir(path), state} {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("example touched %s", target)
		}
	}
	if _, err := invoke(t, "config", "example", "extra"); err == nil {
		t.Fatal("example accepted unexpected arguments")
	}
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
	if out, err := invoke(t, "version"); err != nil || !strings.HasPrefix(out, "abr 1.0.0 (") {
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

func TestCanonicalHostRegistrationAndSubdomainIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	state := filepath.Join(dir, "state")
	paths := []string{"--config", path, "--state-dir", state}
	register := func(name, domain string, flags ...string) (string, error) {
		args := append(append([]string{}, paths...), "register", "--config-only", "--name", name, "--dir", filepath.Join(dir, name), "--type", "laravel", "--domain", domain)
		return invoke(t, append(args, flags...)...)
	}
	if _, err := register("site", "example.com", "--canonical-host", "www", "--alias", "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := register("api", "api.example.com"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Apps[0].Domain != "www.example.com" || len(c.Apps[0].Aliases) != 1 || c.Apps[0].Aliases[0] != "example.com" || c.Apps[1].Domain != "api.example.com" || len(c.Apps[1].Aliases) != 0 {
		t.Fatalf("unexpected app domains: %+v", c.Apps)
	}
	beforeConfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforePorts, err := os.ReadFile(filepath.Join(state, "ports.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		domain string
		flags  []string
	}{
		{"example.com", []string{"--canonical-host", "non-www"}},
		{"other.example.com", []string{"--canonical-host", "invalid"}},
		{"other.example.com", []string{"--canonical-host", "www", "--serving-domain", "other.example.com"}},
	} {
		if out, err := register("other", tt.domain, tt.flags...); err == nil || out != "" {
			t.Fatalf("conflicting registration succeeded: %s, %v", out, err)
		}
		afterConfig, _ := os.ReadFile(path)
		afterPorts, _ := os.ReadFile(filepath.Join(state, "ports.json"))
		if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforePorts, afterPorts) {
			t.Fatal("failed domain registration changed saved config or reservations")
		}
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
