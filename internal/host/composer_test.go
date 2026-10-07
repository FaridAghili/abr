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

func TestSharedComposerCredentialsPersistMergeAndStayPrivate(t *testing.T) {
	h, runner, out, a := fixture(t)
	if err := h.ComposerAuth("nova.laravel.com", "fixture@example.invalid", "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if err := h.ComposerAuth("packages.example.invalid", "other", "other-token"); err != nil {
		t.Fatal(err)
	}
	// A fresh Host reads both saved repositories for another app and repeated installs.
	for _, user := range []string{a.User, "abr-another", a.User} {
		a.User = user
		fresh := h
		if _, err := fresh.asUser(a, nil, true, "composer", "install"); err != nil {
			t.Fatal(err)
		}
		cmd := runner.calls[len(runner.calls)-1]
		if !cmd.Private || !cmd.Stream || !slices.Contains(cmd.Args, "COMPOSER_HOME="+h.composerDir()) || !slices.Contains(cmd.Args, "COMPOSER_CACHE_DIR=/var/lib/abr-users/"+user+"/.cache/composer") {
			t.Fatalf("Composer did not use shared auth and its own cache: %+v", cmd)
		}
	}
	auth, configured, err := h.sharedComposer()
	if err != nil || !configured || auth.HTTPBasic["nova.laravel.com"].Password != "fixture-token" || auth.HTTPBasic["packages.example.invalid"].Password != "other-token" {
		t.Fatalf("credentials not retained: configured=%v err=%v", configured, err)
	}
	path := filepath.Join(h.composerDir(), "auth.json")
	info, err := os.Stat(h.path(path))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe credentials permissions: %v", err)
	}
	for _, cmd := range runner.calls {
		if strings.Contains(strings.Join(cmd.Args, " "), "fixture-token") || strings.Contains(string(cmd.Input), "fixture-token") {
			t.Fatal("token entered subprocess arguments/input")
		}
	}
	if strings.Contains(out.String(), "fixture-token") || strings.Contains(out.String(), "other-token") {
		t.Fatal("credential leaked to output")
	}
	if err := os.Chmod(h.path(path), 0640); err != nil {
		t.Fatal(err)
	}
	if err := h.ComposerAuth("nova.laravel.com", "fixture@example.invalid", "replacement"); err != nil {
		t.Fatal(err)
	}
	auth, _, err = h.sharedComposer()
	if err != nil || auth.HTTPBasic["nova.laravel.com"].Password != "replacement" || auth.HTTPBasic["packages.example.invalid"].Password != "other-token" {
		t.Fatal("updating one repository discarded other credentials")
	}
}

func TestSharedComposerRejectsUnsafeOrCorruptSecretsBeforeCommands(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "invalid", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, out, a := fixture(t)
			if err := h.ComposerAuth("nova.laravel.com", "fixture", "fixture-token"); err != nil {
				t.Fatal(err)
			}
			path := h.path(filepath.Join(h.composerDir(), "auth.json"))
			switch kind {
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(a.Directory, ".env"), path); err != nil {
					t.Fatal(err)
				}
			case "invalid", "oversized":
				data := []byte(`{"http-basic":{"nova.laravel.com":{"password":"fixture-token"}}}`)
				if kind == "oversized" {
					data = bytes.Repeat([]byte("x"), (64<<10)+1)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.asUser(a, nil, true, "composer", "install"); err == nil || len(runner.calls) != 0 || strings.Contains(err.Error()+out.String(), "fixture-token") {
				t.Fatal("unsafe credentials used or exposed")
			}
		})
	}
}

func TestSharedComposerRemovalRevokesAccessAndPreservesCredentials(t *testing.T) {
	h, runner, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.ComposerAuth("nova.laravel.com", "fixture", "fixture-token"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.composerDir(), "auth.json")
	before, _ := h.read(path)
	if err := h.Remove(a.Name); err != nil {
		t.Fatal(err)
	}
	after, err := h.read(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("removal changed shared credentials")
	}
	revoked, deleted := -1, -1
	for i, cmd := range runner.calls {
		if cmd.Name == "setfacl" && cmd.Args[1] == "u:"+a.User+":---" && cmd.Args[len(cmd.Args)-1] == path {
			revoked = i
		}
		if cmd.Name == "userdel" {
			deleted = i
		}
	}
	if revoked < 0 || deleted <= revoked {
		t.Fatal("deleted user before revoking its credential ACL")
	}
}

func TestComposerFailureIdentifiesCommandAndSuggestsAuthentication(t *testing.T) {
	h, runner, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	h.Runner = transferRunner{func(cmd Command) ([]byte, error) {
		if cmd.Name == "runuser" && slices.Contains(cmd.Args, "composer") {
			return []byte("SQL and passwords must remain private: fixture-token"), fmt.Errorf("runuser failed: %w", testExit(100))
		}
		return runner.Run(cmd)
	}}
	err := h.Deploy([]string{a.Name}, DeployOptions{NoPull: true})
	if err == nil || !strings.Contains(err.Error(), "composer install") || !strings.Contains(err.Error(), "abr composer auth --host HOST") || strings.Contains(out.String()+err.Error(), "fixture-token") {
		t.Fatalf("unsafe or unhelpful deployment failure: %v", err)
	}
}
