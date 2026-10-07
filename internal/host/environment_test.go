package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareEnvCopiesExampleAndSetsPrivateManagedDatabase(t *testing.T) {
	h, runner, out, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Directory, ".env")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	example := "# app settings\nAPP_ENV=local\nAPP_DEBUG=true\nAPP_URL=http://localhost\nAPP_KEY=\nMAIL_TOKEN=example-secret\nDB_CONNECTION=sqlite\nDB_HOST=localhost\nexport DB_PASSWORD=old\nDB_PASSWORD=duplicate\n"
	if err := os.WriteFile(filepath.Join(a.Directory, ".env.example"), []byte(example), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := h.transferCredentials(a)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"APP_ENV": "production", "APP_DEBUG": "false", "APP_URL": "https://" + a.Domain, "MAIL_TOKEN": "example-secret", "DB_CONNECTION": "mysql", "DB_HOST": "127.0.0.1", "DB_PORT": "3306", "DB_DATABASE": c.Database, "DB_USERNAME": c.User, "DB_PASSWORD": c.Password} {
		if got := dotenvValue(data, key); got != want {
			t.Fatalf("%s mismatch", key)
		}
	}
	if strings.Count(string(data), "DB_PASSWORD=") != 1 {
		t.Fatal("duplicate database passwords retained")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("environment is not private", err)
	}
	if strings.Contains(out.String(), c.Password) || strings.Contains(out.String(), "example-secret") {
		t.Fatal("secret printed")
	}
	for _, command := range runner.calls {
		if strings.Contains(strings.Join(command.Args, " "), c.Password) || strings.Contains(strings.Join(command.Args, " "), "example-secret") {
			t.Fatal("secret in command arguments")
		}
		if slices.Contains(command.Args, "tee") && (!command.Private || command.Args[1] != a.User) {
			t.Fatal("environment written with unsafe privileges")
		}
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(second, data) {
		t.Fatal("repeated preparation changed settings")
	}
}

func TestPrepareEnvPreservesExistingSecretsAndFailedWrites(t *testing.T) {
	h, runner, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Directory, ".env")
	data := []byte("APP_KEY=existing-key\nAPP_ENV=custom\nCUSTOM_TOKEN=private\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) {
		t.Fatal("existing environment overwritten")
	}
	runner.fail = func(c Command) error {
		if slices.Contains(c.Args, "tee") {
			return fmt.Errorf("write failed")
		}
		return nil
	}
	if err := h.PrepareEnv(a.Name); err == nil {
		t.Fatal("write failure reported as success")
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, data) {
		t.Fatal("failed write truncated existing environment")
	}
	temps, _ := filepath.Glob(filepath.Join(a.Directory, ".abr-env-*"))
	if len(temps) != 0 {
		t.Fatal("private temporary environment left behind")
	}
}

func TestEnvironmentRejectsSymlinksAndForeignAccounts(t *testing.T) {
	h, runner, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Directory, ".env")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(h.root, "outside-secret")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err == nil {
		t.Fatal("followed environment symlink")
	}
	if _, err := h.EnvEditor(a.Name); err == nil {
		t.Fatal("opened environment symlink")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "unchanged" {
		t.Fatal("outside file changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("APP_KEY=key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner.users[a.User] = strings.Replace(runner.users[a.User], ":991:991:", ":992:992:", 1)
	if err := h.PrepareEnv(a.Name); err == nil {
		t.Fatal("adopted replacement user")
	}
	if _, err := h.EnvEditor(a.Name); err == nil {
		t.Fatal("editor adopted replacement user")
	}
}

func TestNuxtEnvironmentWithoutExampleAndEditorIdentity(t *testing.T) {
	h, _, _, a := fixture(t)
	a.Type = "nuxt"
	a.Web.Driver = ""
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	cmd, err := h.EnvEditor(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/usr/sbin/runuser" || cmd.Args[2] != a.User || !slices.Contains(cmd.Args, "/usr/bin/nano") || cmd.Dir != a.Directory {
		t.Fatal("editor is not an app-user foreground process")
	}
	if len(cmd.Env) != 2 {
		t.Fatal("editor inherited privileged environment")
	}
}

func TestMissingLaravelExampleAndEnvironmentPreview(t *testing.T) {
	h, _, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err == nil {
		t.Fatal("missing Laravel example hidden")
	}
	h.DryRun = true
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Directory, ".env")); !os.IsNotExist(err) {
		t.Fatal("preview created .env")
	}
	if _, err := h.EnvEditor(a.Name); err == nil {
		t.Fatal("preview allowed editing")
	}
	if !strings.Contains(out.String(), "Would copy .env.example") {
		t.Fatal("missing preview")
	}
}
