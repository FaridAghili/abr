package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/backup"
)

type hostKeyRunner struct {
	scan  string
	err   error
	scans int
}

func TestBackupHostKeyRescansBeforeTrustAndRetainsNoUnapprovedIdentity(t *testing.T) {
	h, runner, _ := hostKeyFixture(t, 22)
	key, err := h.BackupHostKey()
	if err != nil {
		t.Fatal(err)
	}
	_, changed, _ := hostKeyFixture(t, 22)
	runner.scan = changed.scan
	if err := h.TrustBackupHostKey(key.Fingerprint); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatal("scan changed after confirmation but was trusted", err)
	}
	runner.err = errors.New("fixture connection failure")
	if _, err := h.BackupHostKey(); err == nil || !strings.Contains(err.Error(), "connectivity") {
		t.Fatal("failed scan reported identity success", err)
	}
	if _, err := os.Stat(h.path("/root/.ssh/known_hosts")); !os.IsNotExist(err) {
		t.Fatal("failed trust/scan created known_hosts")
	}
}

func (r *hostKeyRunner) Run(c Command) ([]byte, error) {
	if c.Name == "ssh-keyscan" {
		r.scans++
		return []byte(r.scan), r.err
	}
	return (ExecRunner{}).Run(c)
}

func hostKeyFixture(t *testing.T, port int) (Host, *hostKeyRunner, string) {
	t.Helper()
	h := freshFullHost(t)
	d := backup.Destination{Host: "192.0.2.10", User: "backup", Directory: "/srv/backups/abr", Port: port, Auth: "key"}
	data, _ := json.Marshal(d)
	if err := h.write(h.backupDestinationPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if _, err := (ExecRunner{}).Run(Command{Name: "ssh-keygen", Args: []string{"-t", "ed25519", "-N", "", "-f", identity}, Private: true}); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(identity + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	address := d.Host
	if port != 22 {
		address = fmt.Sprintf("[%s]:%d", address, port)
	}
	r := &hostKeyRunner{scan: address + " " + strings.TrimSpace(string(public)) + "\n"}
	// ssh-keyscan prints no key comment.
	r.scan = strings.Join(strings.Fields(r.scan)[:3], " ") + "\n"
	h.Runner = r
	return h, r, address
}

func TestBackupHostKeyExplicitTrustPreservesAndRecognizesHashedEntries(t *testing.T) {
	for _, port := range []int{22, 2222} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			h, runner, address := hostKeyFixture(t, port)
			path := h.path("/root/.ssh/known_hosts")
			other := strings.Replace(runner.scan, address, "other.example.invalid", 1)
			if err := h.write(path, []byte(other), 0600); err != nil {
				t.Fatal(err)
			}
			key, err := h.BackupHostKey()
			if err != nil || key.Known || key.Address != address {
				t.Fatal("inspection trusted an unknown key", key, err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != other {
				t.Fatal("inspection wrote known_hosts")
			}
			fingerprints, err := (ExecRunner{}).Run(Command{Name: "ssh-keygen", Args: []string{"-l", "-E", "sha256", "-f", path}, Private: true})
			if err != nil || !strings.Contains(string(fingerprints), key.Fingerprint) {
				t.Fatal("fingerprint differs from OpenSSH", err)
			}
			if err := h.TrustBackupHostKey("SHA256:" + strings.Repeat("A", 43)); err == nil {
				t.Fatal("unconfirmed fingerprint accepted")
			}
			if err := h.TrustBackupHostKey(key.Fingerprint); err != nil {
				t.Fatal(err)
			}
			data, _ = os.ReadFile(path)
			if string(data) != other+runner.scan {
				t.Fatal("lost existing known_hosts entries")
			}
			if _, err := (ExecRunner{}).Run(Command{Name: "ssh-keygen", Args: []string{"-H", "-f", path}, Private: true}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			key, err = h.BackupHostKey()
			if err != nil || !key.Known {
				t.Fatal("hashed host key not recognized", key, err)
			}
			if err := h.TrustBackupHostKey(key.Fingerprint); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("known host key duplicated or rewritten")
			}
		})
	}
}

func TestBackupHostKeyRefusesChangedKeyAndUnsafeFile(t *testing.T) {
	h, runner, _ := hostKeyFixture(t, 22)
	key, err := h.BackupHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.TrustBackupHostKey(key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	path := h.path("/root/.ssh/known_hosts")
	before, _ := os.ReadFile(path)
	_, other, _ := hostKeyFixture(t, 22)
	runner.scan = other.scan
	changed, err := h.BackupHostKey()
	if err == nil || !strings.Contains(err.Error(), "differs from known_hosts") {
		t.Fatal("changed host key accepted", err)
	}
	if err := h.TrustBackupHostKey(changed.Fingerprint); err == nil {
		t.Fatal("changed key replaced existing trust")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("changed key modified known_hosts")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("preserve\n"), 0600)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.BackupHostKey(); err == nil {
		t.Fatal("known_hosts symlink accepted")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "preserve\n" {
		t.Fatal("symlink target modified")
	}
}

func TestBackupHostKeyRejectsInvalidScanAndPreviewMakesNoConnections(t *testing.T) {
	h, runner, _ := hostKeyFixture(t, 22)
	valid := runner.scan
	for _, scan := range []string{"", "unexpected ssh-ed25519 invalid\n", strings.Replace(valid, "ssh-ed25519", "ssh-rsa", 1), valid + "192.0.2.10 ssh-ed25519 invalid\n"} {
		runner.scan = scan
		if _, err := h.BackupHostKey(); err == nil {
			t.Fatal("invalid scan accepted")
		}
	}
	runner.scan = valid
	key, err := h.BackupHostKey()
	if err != nil {
		t.Fatal(err)
	}
	before := runner.scans
	if err := h.TrustBackupHostKey(""); err == nil || runner.scans != before {
		t.Fatal("missing approval connected or succeeded")
	}
	h.DryRun = true
	if _, err := h.BackupHostKey(); err != nil {
		t.Fatal(err)
	}
	if err := h.TrustBackupHostKey(key.Fingerprint); err != nil || runner.scans != before {
		t.Fatal("preview connected", err)
	}
	if _, err := os.Stat(h.path("/root/.ssh/known_hosts")); !os.IsNotExist(err) {
		t.Fatal("preview saved trust")
	}
}
