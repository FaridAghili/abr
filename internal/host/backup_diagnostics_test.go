package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"abr/internal/backup"
)

func TestBackupDestinationExplainsRemoteFailuresWithoutLeakingDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, result, want string
	}{
		{"mode", "mode 750", "has mode 0750; required mode is 0700"},
		{"special-mode", "mode 1700", "has mode 1700; required mode is 0700"},
		{"owner", "owner 1000 1001", "owned by UID 1000; SSH user backup has UID 1001"},
		{"missing", "missing", "does not exist"},
		{"not-directory", "not-directory", "is not a directory"},
		{"parent-file", "parent-not-directory", "parent path that is not a directory"},
		{"parent-access", "parent-access", "lacks execute permission on a parent directory"},
		{"symlink", "symlink", "resolves through a symlink"},
		{"access", "access", "cannot be written or accessed"},
		{"write", "write", "could not create or write a test file"},
		{"cleanup", "cleanup", "could not remove the temporary test file"},
		{"invalid-mode", "mode private-fixture-secret", "invalid directory failure result"},
		{"invalid-owner", "owner private-fixture-secret 1001", "invalid directory failure result"},
		{"invalid-result", "unknown private-fixture-secret", "invalid directory failure result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := freshFullHost(t)
			d := backup.Destination{Host: "192.0.2.10", User: "backup", Directory: "/srv/private backups", Port: 2222, Auth: "password", Password: "private-fixture-secret"}
			data, _ := json.Marshal(d)
			if err := h.write(h.backupDestinationPath(), data, 0600); err != nil {
				t.Fatal(err)
			}
			h.Runner = transferRunner{func(c Command) ([]byte, error) {
				if c.Name != "sshpass" || !c.Private || c.Stderr == nil || len(c.ExtraFiles) != 1 {
					t.Fatal("unsafe destination test command")
				}
				if strings.Contains(strings.Join(c.Args, " ")+strings.Join(c.Env, " "), d.Password) {
					t.Fatal("password in process arguments/environment")
				}
				io.WriteString(c.Stderr, "private-fixture-secret\n")
				return []byte("ABR_DESTINATION_ERROR " + tc.result + "\n"), errors.New("fixture exit status 1")
			}}
			err := h.TestBackupDestination()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("destination failure not explained", err)
			}
			text := h.Output.(*bytes.Buffer).String() + err.Error()
			if strings.Contains(text, d.Password) || strings.Contains(text, "destination verified") {
				t.Fatal("private data leaked or failed check reported success")
			}
			if tc.name == "mode" && !strings.Contains(err.Error(), "chmod 700 '/srv/private backups'") {
				t.Fatal("missing safely quoted repair command", err)
			}
			files, _ := os.ReadDir(h.Manager.StateDir)
			for _, f := range files {
				if strings.HasPrefix(f.Name(), ".ssh-password-") {
					t.Fatal("password descriptor file retained after failure")
				}
			}
		})
	}
}

func TestBackupSSHExplainsConnectionFailuresWithoutLoggingStderr(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, password, want string
	}{
		{"unknown-key", "No ED25519 host key is known and you have requested strict checking.\nHost key verification failed.", "", "SSH host key is not trusted"},
		{"changed-key", "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed.", "", "SSH host key has changed"},
		{"key-auth", "backup@fixture: Permission denied (publickey).", "", "SSH key authentication failed"},
		{"password-auth", "backup@fixture: Permission denied (password).", "private-fixture-secret", "SSH password authentication failed"},
		{"bad-key", "Load key \"/private/key\": invalid format", "", "SSH private key could not be loaded"},
		{"refused", "ssh: connect to host fixture port 2222: Connection refused", "", "SSH connection was refused"},
		{"timeout", "ssh: connect to host fixture port 2222: Connection timed out", "", "SSH connection timed out"},
		{"routing", "ssh: connect to host fixture port 2222: No route to host", "", "backup server is unreachable"},
		{"dns", "ssh: Could not resolve hostname fixture: nodename nor servname provided", "", "hostname could not be resolved"},
		{"closed", "Connection closed by 192.0.2.10 port 2222", "", "SSH connection was closed unexpectedly"},
		{"space", "mktemp: failed to create file: No space left on device", "", "backup filesystem is out of space"},
		{"quota", "cat: write error: Disk quota exceeded", "", "disk quota is exceeded"},
		{"readonly", "mktemp: failed to create file: Read-only file system", "", "backup filesystem is read-only"},
		{"unknown", "private-fixture-secret", "", "check sshd logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := freshFullHost(t)
			cause := errors.New("fixture exit status 255")
			if err := os.MkdirAll(h.Manager.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			h.Runner = transferRunner{func(c Command) ([]byte, error) {
				if c.Stderr == nil || !c.Private {
					t.Fatal("diagnostic capture missing or not private")
				}
				io.WriteString(c.Stderr, "private-fixture-secret\n"+tc.stderr)
				return nil, cause
			}}
			_, err := h.backupSSH(BackupOptions{Remote: "backup@192.0.2.10", SSHPort: 2222, Password: tc.password}, "true", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "SSH port 2222") || !errors.Is(err, cause) {
				t.Fatal("connection failure not explained", err)
			}
			if strings.Contains(err.Error()+h.Output.(*bytes.Buffer).String(), "private-fixture-secret") {
				t.Fatal("raw SSH diagnostic leaked")
			}
		})
	}
}

func TestPrivateRunnerCapturesDiagnosticsSeparately(t *testing.T) {
	var logs bytes.Buffer
	var diagnostics commandOutput
	result, err := (ExecRunner{Output: &logs}).Run(Command{
		Name: "sh", Args: []string{"-c", "printf 'receipt\\n'; printf 'private-fixture-secret\\n' >&2; exit 5"},
		Private: true, Stderr: &diagnostics,
	})
	if err == nil || string(result) != "receipt\n" || string(diagnostics.Bytes()) != "private-fixture-secret\n" || logs.Len() != 0 || strings.Contains(err.Error(), "private-fixture-secret") {
		t.Fatal("private diagnostics polluted results, logs or errors", err)
	}
	classified := backupSSHError(BackupOptions{Remote: "backup@fixture", Password: "private-fixture-secret"}, nil, err)
	if !strings.Contains(classified.Error(), "password authentication failed") {
		t.Fatal("sshpass exit status 5 not explained", classified)
	}
}

func TestBackupDestinationRequiresReceiptAndSuccessfulSSHExit(t *testing.T) {
	h := freshFullHost(t)
	d := backup.Destination{Host: "192.0.2.10", User: "backup", Directory: "/srv/backups/abr", Port: 22, Auth: "password", Password: "fixture-secret"}
	data, _ := json.Marshal(d)
	if err := h.write(h.backupDestinationPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		output string
		err    error
	}{
		{"", nil},
		{"login banner\nABR_DESTINATION_OK\n", nil},
		{"ABR_DESTINATION_OK\n", errors.New("fixture SSH failure")},
		{"ABR_DESTINATION_ERROR mode 755\n", nil},
	} {
		h.Runner = transferRunner{func(Command) ([]byte, error) { return []byte(tc.output), tc.err }}
		if err := h.TestBackupDestination(); err == nil {
			t.Fatal("invalid/failed check reported success")
		}
	}
	h.DryRun = true
	h.Runner = transferRunner{func(Command) ([]byte, error) { t.Fatal("preview connected to server"); return nil, nil }}
	if err := h.TestBackupDestination(); err != nil {
		t.Fatal(err)
	}
}
