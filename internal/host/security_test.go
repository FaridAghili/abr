package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestRunnerRejectsCallerPathAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte("#!/bin/sh\nprintf hijacked"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ABR_CALLER_SECRET", "must-not-inherit")
	t.Setenv("GIT_CONFIG_COUNT", "99")
	data, err := (ExecRunner{}).Run(Command{Name: "sh", Args: []string{"-c", `printf '%s:%s:%s' "${ABR_CALLER_SECRET-unset}" "${GIT_CONFIG_COUNT-unset}" "$APP_ENV"`}, Env: []string{"APP_ENV=production"}, Private: true})
	if err != nil || string(data) != "unset:unset:production" {
		t.Fatalf("inherited caller path or environment: %q %v", data, err)
	}
}

func TestRunnerSerializesStdoutAndStderr(t *testing.T) {
	var out bytes.Buffer
	_, err := (ExecRunner{Output: &out}).Run(Command{Name: "sh", Args: []string{"-c", "i=0; while [ $i -lt 1000 ]; do printf x; printf y >&2; i=$((i+1)); done"}, Stream: true})
	if err != nil || strings.Count(out.String(), "x") != 1000 || strings.Count(out.String(), "y") != 1000 {
		t.Fatalf("concurrent command logs corrupted: %v, %d bytes", err, out.Len())
	}
}

func TestAppCommandsCannotRegainPrivilegesOrInheritIdentity(t *testing.T) {
	h, runner, _, a := fixture(t)
	for _, name := range []string{"composer", "npm", "/usr/bin/php8.5", "/usr/bin/brotli", "git"} {
		if _, err := h.asUser(a, map[string]string{"HOME": "/root", "USER": "root", "LOGNAME": "root", "PATH": "/unsafe"}, true, name, "--version"); err != nil {
			t.Fatal(err)
		}
		c := runner.calls[len(runner.calls)-1]
		if c.Name != "runuser" || c.Args[1] != a.User || !slices.Contains(c.Args, "--no-new-privs") || slices.Contains(c.Args, "HOME=/root") || slices.Contains(c.Args, "USER=root") || slices.Contains(c.Args, "PATH=/unsafe") {
			t.Fatalf("unsafe application execution: %+v", c)
		}
	}
	a.User = "root"
	before := len(runner.calls)
	if _, err := h.asUser(a, nil, false, "composer", "install"); err == nil || len(runner.calls) != before {
		t.Fatal("root application command was executed")
	}
}

func TestRuntimeAccountRejectsRootIDsBeforeChangingFiles(t *testing.T) {
	for _, ids := range []string{"0:991", "991:0", "00:991", "invalid:991"} {
		t.Run(ids, func(t *testing.T) {
			h, runner, _, a := fixture(t)
			record := userRecord{App: a.Name, User: a.User, Home: "/var/lib/abr-users/" + a.User}
			data, _ := json.Marshal(record)
			if err := h.write(h.userPath(a), data, 0600); err != nil {
				t.Fatal(err)
			}
			runner.users[a.User] = a.User + ":x:" + ids + ":abr-" + a.Name + ":" + record.Home + ":/usr/sbin/nologin"
			if err := h.ensureUser(a); err == nil {
				t.Fatal("adopted privileged account")
			}
			for _, c := range runner.calls {
				if c.Name != "getent" {
					t.Fatal("changed files before rejecting account", c)
				}
			}
		})
	}
}

func TestManagedStateRejectsWritableFilesAndSymlinks(t *testing.T) {
	h, _, _, a := fixture(t)
	path := h.userPath(a)
	if err := h.write(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := h.read(path); err == nil {
		t.Fatal("read app-writable managed state")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.Directory, ".env"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.read(path); err == nil {
		t.Fatal("followed managed state symlink")
	}
}

func TestProjectEnvRejectsSymlinksFIFOsAndOversizedFiles(t *testing.T) {
	_, _, _, a := fixture(t)
	path := filepath.Join(a.Directory, ".env")
	if _, err := readProjectEnv(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.Directory, "composer.lock"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectEnv(path); err == nil {
		t.Fatal("followed project env symlink")
	}
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectEnv(path); err == nil {
		t.Fatal("read project env FIFO")
	}
	os.Remove(path)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectEnv(path); err == nil {
		t.Fatal("read oversized project env")
	}
}

func TestDatabaseGrantsScopeNewAccountsToLiteralNames(t *testing.T) {
	for _, partial := range []string{"0", "1", "unexpected"} {
		t.Run(partial, func(t *testing.T) {
			h, runner, _, a := fixture(t)
			a.Database.Enabled = true
			h.Runner = transferRunner{func(c Command) ([]byte, error) {
				if string(c.Input) == "SELECT @@partial_revokes;\n" {
					return []byte(partial + "\n"), nil
				}
				return runner.Run(c)
			}}
			_, err := h.Register(a, nil)
			if partial == "unexpected" {
				if err == nil {
					t.Fatal("unknown grant semantics accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "abr_app"
			if partial == "0" {
				want = `abr\_app`
			}
			for _, c := range runner.calls {
				if strings.Contains(string(c.Input), "GRANT ALL") {
					if !c.Private || len(c.Args) == 0 || strings.Contains(string(c.Input), "REVOKE") || !strings.Contains(string(c.Input), "ON `"+want+"`.*") {
						t.Fatal("unsafe new-account grants")
					}
					return
				}
			}
			t.Fatal("new account was not granted access to its database")
		})
	}
}

func TestProjectRefusesWritableParentBeforeCommands(t *testing.T) {
	h, runner, _, a := fixture(t)
	if err := os.Chmod(h.AppsDir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Register(a, nil); err == nil {
		t.Fatal("accepted a replaceable project directory")
	}
	if len(runner.calls) != 0 {
		t.Fatal("ran privileged commands before checking project ancestors")
	}
}

func TestManagedHomeSymlinkRefusedBeforePermissionChanges(t *testing.T) {
	h, runner, _, a := fixture(t)
	home := "/var/lib/abr-users/" + a.User
	if err := os.Remove(h.path(home)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.Directory, h.path(home)); err != nil {
		t.Fatal(err)
	}
	if err := h.prepareHome(a.User, home); err == nil || len(runner.calls) != 0 {
		t.Fatal("followed managed-home symlink")
	}
}

func TestPublicTreeACLsRunWithoutRootPrivileges(t *testing.T) {
	h, runner, _, a := fixture(t)
	if err := h.permissions(a); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range runner.calls {
		if c.Name == "setfacl" && c.Args[len(c.Args)-1] != a.Directory {
			t.Fatal("root modified an app-controlled subtree")
		}
		if c.Name == "runuser" && strings.Contains(strings.Join(c.Args, " "), "setfacl") {
			found = true
			if c.Args[1] != a.User {
				t.Fatal("ACL command ran under unexpected account")
			}
		}
	}
	if !found {
		t.Fatal("no unprivileged ACL commands")
	}
}

func TestRunnerStreamsLargeOutputAndBoundsCapturedResults(t *testing.T) {
	command := Command{Name: "sh", Args: []string{"-c", fmt.Sprintf("head -c %d /dev/zero", maxCommandOutput+1)}, Stream: true}
	r := ExecRunner{Output: io.Discard}
	data, err := r.Run(command)
	if err != nil || len(data) != 0 {
		t.Fatalf("streamed output was retained: %d bytes, %v", len(data), err)
	}
	command.Stream, command.Private = false, true
	if _, err := r.Run(command); err == nil || !strings.Contains(err.Error(), "capture limit") {
		t.Fatal("oversized captured result reported success", err)
	}
	var out bytes.Buffer
	r.Output = &out
	command.Stream = true
	if _, err := r.Run(command); err != nil || out.Len() != 0 {
		t.Fatal("private stream leaked", err)
	}
}

func TestBackupRefusesReplaceableParent(t *testing.T) {
	h, _ := transferFixture(t)
	dir := filepath.Join(h.root, "writable", "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(dir), 0777); err != nil {
		t.Fatal(err)
	}
	if err := h.BackupDatabases([]string{"app"}, false, dir); err == nil {
		t.Fatal("accepted a replaceable backup directory")
	}
}

func TestDeploymentKeepsArtisanAndComposerOutputPrivate(t *testing.T) {
	h, runner, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	runner.fail = func(c Command) error {
		if c.Name == "runuser" && strings.Contains(strings.Join(c.Args, " "), "artisan migrate") {
			return testExit(1)
		}
		return nil
	}
	if err := h.Deploy([]string{a.Name}, DeployOptions{NoPull: true}); err == nil {
		t.Fatal("migration failure reported success")
	}
	found := false
	for _, c := range runner.calls {
		args := strings.Join(c.Args, " ")
		if c.Name == "runuser" && (strings.Contains(args, "artisan ") || strings.Contains(args, "composer ") || strings.Contains(args, "npm ")) {
			found = true
			if !c.Private || !c.Stream {
				t.Fatal("deployment could log SQL/password exception output")
			}
		}
	}
	if !found {
		t.Fatal("deployment did not reach PHP commands")
	}
}

func TestGitKeyReadsAreBoundedAndRefuseSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), (64<<10)+1), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(large, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{large, link, fifo, dir} {
		if _, err := readGitKey(path); err == nil {
			t.Fatal("unsafe SSH key accepted", path)
		}
	}
}

func TestManagedWritesRefuseUnsafeParents(t *testing.T) {
	h, _, _, _ := fixture(t)
	outside := filepath.Join(h.root, "outside")
	if err := os.Mkdir(outside, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := h.write(filepath.Join(link, "new", "file"), []byte("secret"), 0600); err == nil {
		t.Fatal("wrote through a parent symlink")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal("changed symlink target", entries, err)
	}
	if err := os.Chmod(outside, 0777); err != nil {
		t.Fatal(err)
	}
	if err := h.write(filepath.Join(outside, "file"), []byte("secret"), 0600); err == nil {
		t.Fatal("wrote into an untrusted directory")
	}
}
