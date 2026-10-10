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

func TestBackupTrustRequiresFingerprintAndPreviewDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--dry-run"}
	for _, args := range [][]string{{"backup", "trust"}, {"backup", "trust", "--fingerprint", "invalid"}} {
		if _, err := invoke(t, append(paths, args...)...); err == nil || !strings.Contains(err.Error(), "--fingerprint") {
			t.Fatal("host trust accepted missing/invalid approval", err)
		}
	}
	out, err := invoke(t, append(paths, "backup", "trust", "--fingerprint", "SHA256:"+strings.Repeat("A", 43))...)
	if err != nil || !strings.Contains(out, "no connection made or known_hosts changed") {
		t.Fatal("host trust preview failed", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("preview created host state")
	}
	if _, err := invoke(t, append(paths, "backup", "host-key", "unexpected")...); err == nil {
		t.Fatal("host-key accepted unexpected args")
	}
}

func TestEnvironmentCommandIsScriptableAndPreviewDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps")}
	if _, err := invoke(t, append(paths, "register", "--config-only", "--name", "app", "--type", "laravel", "--domain", "example.com")...); err != nil {
		t.Fatal(err)
	}
	out, err := invoke(t, append(paths, "env", "app", "--dry-run")...)
	if err != nil || !strings.Contains(out, "Would copy .env.example") {
		t.Fatalf("environment preview: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "app", ".env")); !os.IsNotExist(err) {
		t.Fatal("preview created environment")
	}
	for _, args := range [][]string{{"env"}, {"env", "app", "extra"}} {
		if _, err := invoke(t, append(paths, args...)...); err == nil || !strings.Contains(err.Error(), "use abr env APP") {
			t.Fatal("environment command accepted invalid arguments")
		}
	}
}

func TestSetupHostnamePromptAndScriptableFlag(t *testing.T) {
	var prompt bytes.Buffer
	name, err := setupHostname(strings.NewReader("Invalid name\nmy-vps\n"), &prompt, "", true)
	if err != nil || name != "my-vps" || !strings.Contains(prompt.String(), "Example: my-vps") || !strings.Contains(prompt.String(), "GitHub SSH key") {
		t.Fatalf("hostname guidance: %q %v", prompt.String(), err)
	}
	if _, err := setupHostname(strings.NewReader(""), &prompt, "", false); err == nil {
		t.Fatal("noninteractive setup did not require hostname")
	}
	for _, bad := range []string{"Upper", "vps-", "vps\nnext"} {
		if _, err := setupHostname(nil, &prompt, bad, false); err == nil {
			t.Fatalf("accepted invalid flag: %q", bad)
		}
	}
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state"), "--dry-run"}
	out, err := invoke(t, append(paths, "setup", "--hostname", "my-vps")...)
	if err != nil || !strings.Contains(out, "hostnamectl hostname my-vps") || !strings.Contains(out, "TablePlus") {
		t.Fatalf("setup flag preview: %q %v", out, err)
	}
	if _, err := invoke(t, append(paths, "setup")...); err == nil {
		t.Fatal("scripted setup silently chose a hostname")
	}
	out, err = invoke(t, append(paths, "database", "--admin", "--show")...)
	if err != nil || !strings.Contains(out, "no password displayed") {
		t.Fatalf("admin preview: %q %v", out, err)
	}
	if _, err := invoke(t, append(paths, "database", "an-app", "--admin")...); err == nil {
		t.Fatal("admin option accepted an application")
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("preview created state")
	}
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

func TestFullRemovalRequiresExplicitConfirmationBeforeHostWork(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"remove", "example", "--purge"},
		{"remove", "example", "--yes"},
	} {
		paths := []string{"--config", filepath.Join(dir, "missing.toml"), "--state-dir", filepath.Join(dir, "state")}
		out, err := invoke(t, append(paths, args...)...)
		if err == nil || out != "" || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("removal skipped confirmation: %s %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
			t.Fatal("unconfirmed command touched host state")
		}
	}
}

func TestCloneAndRegistrationUseAppNameUnderAppsDirectory(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(strconv.FormatBool(custom), func(t *testing.T) {
			dir := t.TempDir()
			configPath, state := filepath.Join(dir, "config.toml"), filepath.Join(dir, "state")
			paths := []string{"--config", configPath, "--state-dir", state}
			appsDir := "/srv/apps"
			if custom {
				appsDir = filepath.Join(dir, "projects")
				paths = append(paths, "--apps-dir", appsDir)
			}
			out, err := invoke(t, append(append([]string{}, paths...), "clone", "git@github.com:owner/project.git", "example-api", "--dry-run")...)
			if err != nil || !strings.Contains(out, "into "+filepath.Join(appsDir, "example-api")) {
				t.Fatalf("clone did not resolve its destination: %s %v", out, err)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("clone preview wrote state")
			}
			_, err = invoke(t, append(append([]string{}, paths...), "register", "--config-only", "--name", "example-api", "--type", "laravel", "--domain", "api.example.com")...)
			if err != nil {
				t.Fatal(err)
			}
			c, err := config.Load(configPath)
			if err != nil || len(c.Apps) != 1 || c.Apps[0].Directory != filepath.Join(appsDir, "example-api") {
				t.Fatalf("registration used a different project path: %+v %v", c.Apps, err)
			}
		})
	}
}

func TestCloneRefusesPathsAndInvalidAppNames(t *testing.T) {
	for _, name := range []string{"../escape", "nested/app", "/srv/apps/api", ".", "..", "Uppercase", "app\nname", ""} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			out, err := invoke(t, "--state-dir", state, "clone", "git@github.com:owner/project.git", name, "--dry-run")
			if err == nil || out != "" || !strings.Contains(err.Error(), "application name") {
				t.Fatalf("unsafe app name accepted: %q: %s %v", name, out, err)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("invalid clone name created state")
			}
		})
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
	if out, err := invoke(t, "version"); err != nil || !strings.HasPrefix(out, "abr "+abr.Version+" (") {
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
	for _, command := range [][]string{{"git", "setup"}, {"git", "setup", "--key", "/nonexistent/key"}, {"clone", "git@github.com:owner/repo.git", "repo"}} {
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

func TestComposerAuthenticationPreviewAndPrivateStdin(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	out, err := invoke(t, "composer", "auth", "--state-dir", state, "--dry-run")
	if err != nil || !strings.Contains(out, "Would save shared Composer credentials") {
		t.Fatalf("credential preview: %s %v", out, err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("preview saved credentials")
	}
	for _, args := range [][]string{{"composer"}, {"composer", "unknown"}, {"composer", "auth", "extra"}, {"composer", "auth", "--token", "secret"}} {
		if _, err := invoke(t, args...); err == nil {
			t.Fatalf("invalid credential command accepted: %v", args)
		}
	}
	var prompts bytes.Buffer
	for _, token := range []string{"fixture-token", "fixture-token\n", "fixture-token\r\n"} {
		username, password, err := composerLogin(strings.NewReader(token), &prompts, "fixture", true, false)
		if err != nil || username != "fixture" || password != "fixture-token" || prompts.Len() != 0 {
			t.Fatal("stdin token not read privately")
		}
	}
	for _, token := range []string{"", "token\nextra", strings.Repeat("x", 4097)} {
		if _, _, err := composerLogin(strings.NewReader(token), &prompts, "fixture", true, false); err == nil {
			t.Fatal("invalid stdin token accepted")
		}
	}
	if _, _, err := composerLogin(strings.NewReader("token"), &prompts, "", true, false); err == nil {
		t.Fatal("stdin read without username")
	}
	if _, _, err := composerLogin(strings.NewReader("token"), &prompts, "fixture", false, false); err == nil {
		t.Fatal("nonterminal token prompt allowed")
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

func TestServerUpdateIsScriptableAndPreviewDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	out, err := invoke(t, "update", "--dry-run", "--config", filepath.Join(dir, "config.toml"), "--state-dir", state)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"full-upgrade", "autoremove", "autoclean", "composer self-update", "ncu -g", "no commands executed"} {
		if !strings.Contains(out, operation) {
			t.Fatalf("update preview missing %q: %s", operation, out)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("update preview wrote state")
	}
	if _, err := invoke(t, "update", "extra", "--dry-run"); err == nil {
		t.Fatal("update accepted an application argument")
	}
}

func TestEmbeddingCLIRegistrationEditAndClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	flags := []string{"--config", path, "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps")}
	call := func(args ...string) {
		t.Helper()
		if _, err := invoke(t, append(flags, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	call("register", "--config-only", "--name", "app", "--type", "nuxt", "--domain", "app.example.com", "--embed-path", "/banner.html", "--embed-path", "/ads/*", "--embed-origin", "*")
	call("edit", "app", "--config-only", "--embed-origin", "self", "--embed-origin", "https://partner.example.com")
	c, err := config.Load(path)
	if err != nil || len(c.Apps[0].Embedding.Paths) != 2 || len(c.Apps[0].Embedding.Origins) != 2 {
		t.Fatalf("lost settings: %+v %v", c, err)
	}
	if _, err := invoke(t, append(flags, "edit", "app", "--config-only", "--embed-path", "")...); err == nil {
		t.Fatal("accepted partially cleared embedding")
	}
	call("edit", "app", "--config-only", "--embed-path", "", "--embed-origin", "")
	c, err = config.Load(path)
	if err != nil || len(c.Apps[0].Embedding.Paths) != 0 || len(c.Apps[0].Embedding.Origins) != 0 {
		t.Fatal("clear failed", err)
	}
}

func TestFullTransferCLIValidationAndBackupPreview(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	args := append(append([]string{}, paths...), "register", "--config-only", "--name", "demo", "--dir", "/srv/apps/demo", "--type", "laravel", "--domain", "demo.test")
	if _, err := invoke(t, args...); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "full.tar.gz")
	out, err := invoke(t, append(append([]string{}, paths...), "backup", "--output", output, "--dry-run")...)
	if err != nil || !strings.Contains(out, "full Laravel storage/app and saved source") || !strings.Contains(out, "Redis") {
		t.Fatal("incomplete backup preview", out, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("backup preview wrote archive")
	}
	for _, command := range [][]string{{"backup"}, {"backup", "unexpected", "--output", output}, {"backup", "--output", "relative.tar.gz"}, {"restore"}, {"restore", output, "extra"}, {"restore", output}, {"restore", output, "--yes", "--ssh-port", "70000"}} {
		out, err := invoke(t, append(append([]string{}, paths...), command...)...)
		if err == nil || out != "" {
			t.Fatal("invalid transfer reported success", command, out, err)
		}
	}
	help, err := invoke(t, "help")
	if err != nil || !strings.Contains(help, "restore FILE.tar.gz --yes") {
		t.Fatal("full transfer missing from help", err)
	}
}

func TestOnlineAppBackupCLIAndDestinationPreview(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "config.toml"), "--state-dir", filepath.Join(dir, "state")}
	call := func(args ...string) (string, error) {
		return invoke(t, append(append([]string{}, paths...), args...)...)
	}
	if _, err := call("register", "--config-only", "--name", "demo", "--dir", "/srv/apps/demo", "--type", "nuxt", "--domain", "demo.test"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "archives")
	out, err := call("backup", "--all", "--output-dir", output, "--remote", "backup@192.0.2.10", "--remote-dir", "/srv/backups/abr", "--dry-run")
	if err != nil || !strings.Contains(out, "services remain running") || !strings.Contains(out, "verify SHA256") {
		t.Fatal("incomplete online backup preview", out, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("preview created archives")
	}
	out, err = call("backup", "configure", "--host", "192.0.2.10", "--user", "backup", "--path", "/srv/backups/abr", "--dry-run")
	if err != nil || !strings.Contains(out, "no connection tested or settings saved") {
		t.Fatal("destination preview inaccurate", out, err)
	}
	for _, args := range [][]string{
		{"backup", "--all", "demo", "--dry-run"},
		{"backup", "--all", "--output-dir", "relative", "--dry-run"},
		{"backup", "--all", "--remote", "backup@192.0.2.10", "--dry-run"},
		{"backup", "--all", "--remote", "-bad", "--remote-dir", "/srv/backups/abr", "--dry-run"},
		{"backup", "--all", "--transfer", "--dry-run"},
		{"backup", "configure", "--host", "192.0.2.10", "--user", "backup", "--path", "relative", "--dry-run"},
	} {
		if out, err := call(args...); err == nil || out != "" {
			t.Fatal("invalid backup reported success", args, out, err)
		}
	}
}
