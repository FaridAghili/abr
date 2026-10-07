package host

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Only SSH key generation/validation runs for real, offline in temporary paths.
// Host changes, ACLs and Git/network operations remain simulated.
type keyRunner struct{ *fakeRunner }

func (r keyRunner) Run(c Command) ([]byte, error) {
	if c.Name == "ssh-keygen" {
		r.calls = append(r.calls, c)
		return (ExecRunner{}).Run(c)
	}
	return r.fakeRunner.Run(c)
}

func TestSharedGitKeyIsPrivateStableAndImportDoesNotReplace(t *testing.T) {
	h, r, out, _ := fixture(t)
	h.Runner = keyRunner{r}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(h.gitDir(), "id_ed25519")
	data, err := h.read(key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), data) || !strings.Contains(out.String(), "ssh-ed25519 ") {
		t.Fatal("private key leaked or public key missing")
	}
	for _, path := range []string{key, key + ".pub", filepath.Join(h.gitDir(), "known_hosts")} {
		info, err := os.Stat(h.path(path))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("identity permissions: %s %v", path, err)
		}
	}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	if err := h.GitSetup(h.path(key)); err != nil {
		t.Fatal(err)
	}
	// The ACL mask exposes a group read mode bit even though only named app
	// users can read the root-owned key. Root validation must still work.
	if err := os.Chmod(key, 0640); err != nil {
		t.Fatal(err)
	}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := (ExecRunner{}).Run(Command{Name: "ssh-keygen", Args: []string{"-t", "ed25519", "-N", "", "-f", other}, Private: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.GitSetup(other); err == nil {
		t.Fatal("different identity replaced the existing key")
	}
	after, _ := h.read(key)
	if !bytes.Equal(data, after) {
		t.Fatal("key changed")
	}
}

func TestSharedGitRejectsInvalidImportsAndUnsafeFiles(t *testing.T) {
	for _, password := range []string{"invalid", "passphrase"} {
		t.Run(password, func(t *testing.T) {
			h, r, _, _ := fixture(t)
			h.Runner = keyRunner{r}
			source := filepath.Join(t.TempDir(), "source")
			if password == "invalid" {
				if err := os.WriteFile(source, []byte("not a private key"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := (ExecRunner{}).Run(Command{Name: "ssh-keygen", Args: []string{"-t", "ed25519", "-N", password, "-f", source}, Private: true}); err != nil {
				t.Fatal(err)
			}
			if err := h.GitSetup(source); err == nil {
				t.Fatal("unusable key imported")
			}
			if _, err := os.Stat(filepath.Join(h.gitDir(), "id_ed25519")); !os.IsNotExist(err) {
				t.Fatal("invalid import persisted a key")
			}
		})
	}
	h, r, _, _ := fixture(t)
	h.Runner = keyRunner{r}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(h.gitDir(), "id_ed25519")
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sharedGit(); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key+".pub", key); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sharedGit(); err == nil {
		t.Fatal("symlink identity accepted")
	}
}

func TestSharedGitAutomaticallySelectedAndAccessRevokedBeforeUserDeletion(t *testing.T) {
	h, r, _, a := fixture(t)
	h.Runner = keyRunner{r}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"APP_ENV": "production"}
	if _, err := h.asUser(a, environment, false, "git", "pull", "--ff-only"); err != nil {
		t.Fatal(err)
	}
	git := r.calls[len(r.calls)-1]
	if git.Name != "runuser" || !slices.Contains(git.Args, "GIT_SSH_COMMAND="+h.gitSSH()) || !slices.Contains(git.Args, "GIT_TERMINAL_PROMPT=0") || git.Args[1] != a.User {
		t.Fatal("Git did not run as app user with the shared identity")
	}
	if _, ok := environment["GIT_SSH_COMMAND"]; ok {
		t.Fatal("Git identity leaked into runtime/build environment")
	}
	for _, c := range r.calls {
		if c.Name == "setfacl" && strings.HasPrefix(c.Args[1], "u:"+a.User+":") {
			path := c.Args[len(c.Args)-1]
			if path != h.Manager.StateDir && path != h.gitDir() && path != filepath.Join(h.gitDir(), "id_ed25519") && path != filepath.Join(h.gitDir(), "known_hosts") {
				t.Fatalf("ACL exposed other state: %s", path)
			}
		}
	}
	if err := h.Remove(a.Name); err != nil {
		t.Fatal(err)
	}
	revoked, deleted := -1, -1
	for i, c := range r.calls {
		if c.Name == "setfacl" && c.Args[1] == "u:"+a.User+":---" {
			revoked = i
		}
		if c.Name == "userdel" {
			deleted = i
		}
	}
	if revoked < 0 || deleted <= revoked {
		t.Fatal("shared Git access not revoked before user deletion")
	}
	if _, err := h.sharedGit(); err != nil {
		t.Fatalf("removal deleted shared identity: %v", err)
	}
}

func TestCloneUsesSharedIdentityAndRefusesExistingDestinations(t *testing.T) {
	h, r, _, a := fixture(t)
	r.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
	destination := filepath.Join(h.AppsDir, "new-app")
	if err := h.Clone("git@github.com:owner/repo.git", destination); err == nil {
		t.Fatal("clone without identity succeeded")
	}
	h.Runner = keyRunner{r}
	if err := h.GitSetup(""); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ url, path string }{{"https://github.com/owner/repo.git", destination}, {"git@github.com:owner/repo.git", a.Directory}, {"git@github.com:owner/repo.git", "/tmp/outside"}} {
		if err := h.Clone(item.url, item.path); err == nil {
			t.Fatalf("unsafe clone accepted: %+v", item)
		}
	}
	if err := h.Clone("git@github.com:owner/repo.git", destination); err != nil {
		t.Fatal(err)
	}
	var c Command
	for _, command := range r.calls {
		if command.Name == "runuser" && slices.Contains(command.Args, "clone") {
			c = command
		}
	}
	if c.Name != "runuser" || c.Args[1] != "_apt" || !slices.Contains(c.Args, "--no-new-privs") || !strings.Contains(strings.Join(c.Args, " "), "GIT_SSH_COMMAND=ssh -F /dev/null") || !strings.Contains(strings.Join(c.Args, " "), ".abr-clone-") || !slices.Contains(c.Args, "core.hooksPath=/dev/null") || !slices.Contains(c.Args, "--template=") {
		t.Fatal("initial clone missing shared identity or hook restrictions")
	}
	if paths, _ := filepath.Glob(filepath.Join(h.AppsDir, ".abr-clone-*")); len(paths) != 0 {
		t.Fatal("temporary clone credential was not removed")
	}
	r.fail = func(c Command) error {
		if c.Name == "runuser" && slices.Contains(c.Args, "clone") {
			return testExit(7)
		}
		return nil
	}
	if err := h.Clone("git@github.com:owner/repo.git", filepath.Join(h.AppsDir, "failed-app")); err == nil {
		t.Fatal("failed clone reported success")
	}
	if paths, _ := filepath.Glob(filepath.Join(h.AppsDir, ".abr-clone-*")); len(paths) != 0 {
		t.Fatal("failed clone retained temporary identity", paths)
	}
}

func TestGitPreviewDoesNotCreateFiles(t *testing.T) {
	h, _, _, _ := fixture(t)
	h.DryRun = true
	if err := h.GitSetup("/nonexistent/import"); err != nil {
		t.Fatal(err)
	}
	if err := h.Clone("git@github.com:owner/repo.git", filepath.Join(h.AppsDir, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.gitDir()); !os.IsNotExist(err) {
		t.Fatal("preview wrote shared identity")
	}
}
