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
	if err := os.WriteFile(filepath.Join(a.Directory, ".env.example"), []byte("APP_KEY=\n"), 0644); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(a.Directory, ".env.example"), []byte("APP_KEY=\n"), 0644); err != nil {
		t.Fatal(err)
	}
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
	if _, _, err := h.EnvEditor(a.Name); err == nil {
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
	if _, _, err := h.EnvEditor(a.Name); err == nil {
		t.Fatal("editor adopted replacement user")
	}
}

func TestNuxtEnvironmentWithExampleAndEditorIdentity(t *testing.T) {
	h, _, _, a := fixture(t)
	a.Type = "nuxt"
	a.Web.Driver = ""
	if err := os.WriteFile(filepath.Join(a.Directory, ".env.example"), []byte("NUXT_PUBLIC_API_BASE=https://api.example.test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	cmd, changed, err := h.EnvEditor(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil || cmd == nil || cmd.Path != "/usr/sbin/runuser" || cmd.Args[2] != a.User || !slices.Contains(cmd.Args, "/usr/bin/nano") || cmd.Dir != a.Directory {
		t.Fatal("editor is not an app-user foreground process")
	}
	if len(cmd.Env) != 2 {
		t.Fatal("editor inherited privileged environment")
	}
}

func TestMissingExampleAndEnvironmentPreview(t *testing.T) {
	h, _, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	h.DryRun = true
	if err := h.PrepareEnv(a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Directory, ".env")); !os.IsNotExist(err) {
		t.Fatal("preview created .env")
	}
	if _, _, err := h.EnvEditor(a.Name); err == nil {
		t.Fatal("preview allowed editing")
	}
	if !strings.Contains(out.String(), "Would copy .env.example") {
		t.Fatal("missing preview")
	}
}

func TestMissingExampleSkipsPreparationButAllowsEditingExistingEnv(t *testing.T) {
	for _, kind := range []string{"laravel", "nuxt"} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", kind, existing), func(t *testing.T) {
				h, runner, out, a := fixture(t)
				a.Type = kind
				a.Database.Enabled = kind == "laravel"
				if kind == "nuxt" {
					a.Web.Driver = ""
				}
				if _, err := h.Register(a, nil); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(a.Directory, ".env")
				old, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !existing {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				runner.calls = nil
				out.Reset()
				if err := h.PrepareEnv(a.Name); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if existing && (err != nil || !bytes.Equal(got, old)) || !existing && !os.IsNotExist(err) {
					t.Fatal("missing example created or changed .env")
				}
				if len(runner.calls) != 0 || !strings.Contains(out.String(), "Skipped .env preparation") || strings.Contains(out.String(), "Set managed MySQL") {
					t.Fatal("skipped environment performed or claimed work")
				}
				cmd, changed, err := h.EnvEditor(a.Name)
				if existing {
					if err != nil || cmd == nil || changed == nil {
						t.Fatalf("existing .env could not be edited without an example: %v", err)
					}
				} else if err == nil || cmd != nil || changed != nil {
					t.Fatal("missing .env allowed editing")
				}
			})
		}
	}
}

func TestEnvEditorDetectsContentChanges(t *testing.T) {
	h, _, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Directory, ".env")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := h.EnvEditor(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		got, err := changed()
		if err != nil || got != want {
			t.Fatalf("changed = %t, want %t: %v", got, want, err)
		}
	}
	check(false)
	// A save or atomic replacement with identical contents must not deploy.
	temp := filepath.Join(a.Directory, "saved-env")
	if err := os.WriteFile(temp, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	check(false)
	// Content changes still count when length and timestamps are unchanged.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	after := bytes.Clone(before)
	after[len(after)-1] = ' '
	if err := os.WriteFile(path, after, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestEnvEditorChangeCheckRejectsRemovedOrUnsafeFiles(t *testing.T) {
	for _, replacement := range []string{"missing", "symlink", "directory", "oversized"} {
		t.Run(replacement, func(t *testing.T) {
			h, _, _, a := fixture(t)
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			_, changed, err := h.EnvEditor(a.Name)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(a.Directory, ".env")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "symlink":
				err = os.Symlink(filepath.Join(a.Directory, "artisan"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "oversized":
				err = os.WriteFile(path, make([]byte, (1<<20)+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := changed(); err == nil || got {
				t.Fatalf("unsafe .env reported a successful change check: %t, %v", got, err)
			}
		})
	}
}
