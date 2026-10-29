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

	"abr/internal/backup"
)

func (h Host) backupIdentity() string {
	return filepath.Join(h.Manager.StateDir, "backup-ssh", "id_ed25519")
}
func (h Host) backupDestinationPath() string {
	return filepath.Join(h.Manager.StateDir, "backup-destination.json")
}

func (h Host) ConfigureBackup(d backup.Destination, source string) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if source != "" && d.Auth != "key" {
		return fmt.Errorf("a private key cannot be combined with password authentication")
	}
	return h.locked(func() error {
		if h.DryRun {
			h.say("Would save private backup destination %s@%s:%s (SSH port %d, %s authentication); no connection tested or settings saved", d.User, d.Host, d.Directory, d.Port, d.Auth)
			return nil
		}
		var public []byte
		if d.Auth == "key" {
			key := h.path(h.backupIdentity())
			if err := os.MkdirAll(filepath.Dir(key), 0700); err != nil {
				return err
			}
			if err := h.gitFile(filepath.Dir(key), true); err != nil {
				return err
			}
			if err := h.trustedFile(key); err != nil && !os.IsNotExist(err) {
				return err
			}
			candidateDir, err := os.MkdirTemp(filepath.Dir(key), ".identity-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(candidateDir)
			candidate := filepath.Join(candidateDir, "identity")
			if source != "" {
				data, err := readGitKey(source)
				if err != nil {
					return err
				}
				if err := h.write(candidate, data, 0600); err != nil {
					return err
				}
			} else if _, err := os.Lstat(key); err == nil {
				if err := h.gitFile(key, false); err != nil {
					return err
				}
				if err := copyBackupFile(key, candidate, false); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			} else {
				if _, err := h.run("Generate dedicated backup SSH identity", Command{Name: "ssh-keygen", Args: []string{"-t", "ed25519", "-N", "", "-C", "abr-backup", "-f", candidate}, Private: true}); err != nil {
					return err
				}
			}
			public, err = h.publicGitKey(candidate)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				return err
			}
			if err := h.write(key, data, 0600); err != nil {
				return err
			}
			if err := h.write(key+".pub", public, 0600); err != nil {
				return err
			}
		} else if err := h.installSSHPass(); err != nil {
			return err
		}
		if err := h.trustedFile(h.backupDestinationPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		data, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		if err := h.write(h.backupDestinationPath(), append(data, '\n'), 0600); err != nil {
			return err
		}
		h.say("Backup destination saved: %s@%s:%s (SSH port %d). Connection has not been tested.", d.User, d.Host, d.Directory, d.Port)
		if len(public) > 0 {
			h.say("Add this public key to %s's ~/.ssh/authorized_keys on the backup server:\n%s", d.User, strings.TrimSpace(string(public)))
		}
		h.say("On the backup server, create %s owned by %s with mode 0700. Choose Test backup destination in the TUI to confirm its host fingerprint, or use abr backup host-key and abr backup trust --fingerprint SHA256:... in the CLI.", d.Directory, d.User)
		return nil
	})
}

// Passwords go through a private descriptor to the native SSH helper.
func (h Host) installSSHPass() error {
	_, err := h.run("Install native SSH password helper", Command{Name: "apt-get", Args: []string{"-o", "DPkg::Lock::Timeout=120", "install", "-y", "sshpass"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Stream: true})
	return err
}

// stageBackupDestination saves the backup server settings, its dedicated key
// and root's trusted entries for its host key, so a restored server keeps
// sending scheduled backups to the same server without trusting it again.
func (h Host) stageBackupDestination(state string) error {
	d, err := h.loadBackupDestination()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	names := []string{"backup-destination.json"}
	if d.Auth == "key" {
		names = append(names, "backup-ssh/id_ed25519", "backup-ssh/id_ed25519.pub")
	}
	for _, name := range names {
		if err := copyBackupFile(h.path(filepath.Join(h.Manager.StateDir, name)), filepath.Join(state, name), name == "backup-ssh/id_ed25519.pub"); err != nil {
			return err
		}
	}
	lines, err := h.knownBackupHostLines(backupHostAddress(d))
	if err != nil {
		return err
	}
	var trusted []byte
	for _, line := range lines {
		// Markers such as @revoked describe policy, not this server's identity.
		if !strings.HasPrefix(line, "@") {
			trusted = append(trusted, line+"\n"...)
		}
	}
	if len(trusted) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(state, "backup-ssh"), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(state, "backup-ssh/known_hosts"), trusted, 0600)
}

// restoreBackupTrust prepares a restored backup destination for its next
// connection: root trusts the saved host key entries, and password login gets
// its SSH helper.
func (h Host) restoreBackupTrust(stage string) error {
	var d backup.Destination
	data, err := os.ReadFile(filepath.Join(stage, "state/backup-destination.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	if d.Auth == "password" {
		if err := h.installSSHPass(); err != nil {
			return err
		}
	}
	saved, err := os.ReadFile(filepath.Join(stage, "state/backup-ssh/known_hosts"))
	if os.IsNotExist(err) {
		h.say("Backup destination restored: %s@%s:%s. Its host key was not trusted when backed up; confirm it with Test backup destination in the TUI, or abr backup host-key and abr backup trust --fingerprint SHA256:...", d.User, d.Host, d.Directory)
		return nil
	}
	if err != nil {
		return err
	}
	path := h.path("/root/.ssh/known_hosts")
	if err := h.trustedFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	known := strings.Split(string(current), "\n")
	data = current
	for _, line := range strings.Split(strings.TrimSpace(string(saved)), "\n") {
		if slices.Contains(known, line) {
			continue
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, line+"\n"...)
	}
	if len(data) != len(current) {
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
	}
	h.say("Backup destination restored: %s@%s:%s (SSH port %d) with its saved credentials and trusted host key", d.User, d.Host, d.Directory, d.Port)
	return nil
}

func (h Host) loadBackupDestination() (backup.Destination, error) {
	var d backup.Destination
	if err := h.trustedFile(h.backupDestinationPath()); err != nil {
		return d, err
	}
	if err := h.gitFile(h.backupDestinationPath(), false); err != nil {
		return d, err
	}
	info, err := os.Stat(h.path(h.backupDestinationPath()))
	if err != nil {
		return d, err
	}
	if info.Mode().Perm() != 0600 {
		return d, fmt.Errorf("backup destination must have mode 0600")
	}
	data, err := h.read(h.backupDestinationPath())
	if err != nil {
		return d, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return d, fmt.Errorf("invalid backup destination")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return d, fmt.Errorf("invalid backup destination")
	}
	return d, d.Validate()
}

func (h Host) backupOptions(o BackupOptions) (BackupOptions, error) {
	if o.OutputDir == "" {
		o.OutputDir = "/var/backups/abr/apps"
	}
	if o.Remote != "" {
		return o, nil
	}
	d, err := h.loadBackupDestination()
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	o.Remote, o.RemoteDir, o.SSHPort = d.User+"@"+d.Host, d.Directory, d.Port
	if d.Auth == "password" {
		o.Password = d.Password
	} else {
		o.KeyFile = h.path(h.backupIdentity())
		if err := h.gitFile(o.KeyFile, false); err != nil {
			return o, fmt.Errorf("backup SSH private key %s: %w", o.KeyFile, err)
		}
	}
	return o, nil
}

func (h Host) TestBackupDestination() error {
	return h.locked(func() error {
		o, err := h.backupOptions(BackupOptions{})
		if err != nil {
			return err
		}
		if o.Remote == "" {
			return fmt.Errorf("configure a backup destination first")
		}
		if h.DryRun {
			h.say("Would verify SSH access and private remote directory; no connection made")
			return nil
		}
		command := "sh -c " + shellQuote(backupDestinationCheck) + " abr-backup " + shellQuote(o.RemoteDir)
		out, err := h.backupSSH(o, command, nil)
		if checkErr := backupDestinationError(o, out); checkErr != nil {
			if err != nil && strings.TrimSpace(string(out)) == "ABR_DESTINATION_ERROR write" {
				return fmt.Errorf("%w: %v", checkErr, err)
			}
			return checkErr
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "ABR_DESTINATION_OK" {
			return fmt.Errorf("backup server did not return a valid directory check result; check the SSH account's shell and login scripts")
		}
		h.say("Backup destination verified: %s:%s", o.Remote, o.RemoteDir)
		return nil
	})
}
