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
	register := append(append([]string{}, paths...), "register", "--name", "quiet", "--dir", "/srv/quiet", "--user", "quiet", "--type", "laravel", "--domain", "quiet.test", "--queue-workers", "2", "--scheduler")
	out, err := invoke(t, register...)
	if err != nil || !strings.Contains(out, "Registered quiet") {
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
	if out, err := invoke(t, "version"); err != nil || !strings.HasPrefix(out, "sites 0.1.0 (") {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := invoke(t, register...); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	// Path flags may also follow the command.
	args := append([]string{"list"}, paths...)
	if out, err := invoke(t, args...); err != nil || !strings.Contains(out, "quiet") {
		t.Fatalf("%s %v", out, err)
	}
	for _, cmd := range []string{"deploy", "enable", "setup", "remove", "status", "unknown"} {
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
	args := []string{"register", "--config", path, "--state-dir", filepath.Join(dir, "state"), "--name", "octane", "--dir", "/srv/octane", "--user", "octane", "--type", "laravel", "--domain", "octane.test", "--web-driver", "octane", "--port", "octane-http=" + port}
	out, err := invoke(t, args...)
	if err == nil || out != "" || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed registration saved config: %v", err)
	}
}
