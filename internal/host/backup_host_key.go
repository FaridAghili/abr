package host

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"abr/internal/backup"
)

// BackupHostKey fetches only a public host key; it never sends credentials or
// updates known_hosts. Existing hashed entries are checked by OpenSSH itself.
func (h Host) BackupHostKey() (key backup.HostKey, err error) {
	err = h.locked(func() error {
		var e error
		key, e = h.backupHostKey()
		return e
	})
	return key, err
}

func (h Host) backupHostKey() (backup.HostKey, error) {
	d, err := h.loadBackupDestination()
	if err != nil {
		return backup.HostKey{}, err
	}
	address := d.Host
	if d.Port != 22 {
		address = "[" + d.Host + "]:" + strconv.Itoa(d.Port)
	}
	key := backup.HostKey{Address: address}
	if h.DryRun {
		h.say("Would inspect the backup server's SSH host key; no connection made or identity trusted")
		key.Known = true
		return key, nil
	}
	out, err := h.run("Read backup server ED25519 host key", Command{Name: "ssh-keyscan", Args: []string{"-T", "15", "-p", strconv.Itoa(d.Port), "-t", "ed25519", d.Host}, Private: true})
	if err != nil {
		return key, fmt.Errorf("cannot read backup server host key; check its address, SSH port and connectivity: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 3 || fields[0] != address || fields[1] != "ssh-ed25519" || (key.Key != "" && key.Key != fields[2]) {
			return key, fmt.Errorf("unexpected or conflicting backup server host keys")
		}
		key.Key = fields[2]
	}
	key.Fingerprint, err = backup.ED25519Fingerprint(key.Key)
	if err != nil {
		return key, err
	}
	found := false
	for _, path := range []string{"/root/.ssh/known_hosts", "/etc/ssh/ssh_known_hosts"} {
		path = h.path(path)
		if err := h.trustedFile(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return key, err
		}
		if err := h.trustedDirectory(filepath.Dir(path)); err != nil {
			return key, err
		}
		out, err := h.run("Check trusted backup server identity", Command{Name: "ssh-keygen", Args: []string{"-F", address, "-f", path}, Private: true})
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			return key, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			found = true
			if len(fields) == 3 && fields[1] == "ssh-ed25519" && fields[2] == key.Key {
				key.Known = true
			}
		}
	}
	if found && !key.Known {
		return key, fmt.Errorf("backup server host key differs from known_hosts; existing trust preserved. Verify the server identity before changing known_hosts")
	}
	h.say("Backup server %s · ED25519 · %s · trusted: %t", key.Address, key.Fingerprint, key.Known)
	return key, nil
}

// TrustBackupHostKey requires an independently confirmed fingerprint and rescans
// under the host lock so a changed key cannot inherit approval.
func (h Host) TrustBackupHostKey(fingerprint string) error {
	raw, prefixed := strings.CutPrefix(fingerprint, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(raw)
	if !prefixed || err != nil || len(decoded) != 32 || base64.RawStdEncoding.EncodeToString(decoded) != raw {
		return fmt.Errorf("provide --fingerprint SHA256:... verified against the backup server's console")
	}
	return h.locked(func() error {
		if h.DryRun {
			h.say("Would pin the confirmed backup server fingerprint; no connection made or known_hosts changed")
			return nil
		}
		key, err := h.backupHostKey()
		if err != nil {
			return err
		}
		if key.Fingerprint != fingerprint {
			return fmt.Errorf("backup server fingerprint does not match the confirmed fingerprint; no trust saved")
		}
		if key.Known {
			h.say("Backup server host key is already trusted")
			return nil
		}
		path := h.path("/root/.ssh/known_hosts")
		if err := h.trustedFile(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, []byte(key.Address+" ssh-ed25519 "+key.Key+"\n")...)
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
		h.say("Backup server host key trusted: %s · %s", key.Address, key.Fingerprint)
		return nil
	})
}
