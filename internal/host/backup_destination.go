package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		} else {
			// Passwords go through a private descriptor to the native SSH helper.
			if _, err := h.run("Install native SSH password helper", Command{Name: "apt-get", Args: []string{"-o", "DPkg::Lock::Timeout=120", "install", "-y", "sshpass"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Stream: true}); err != nil {
				return err
			}
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
		h.say("On the backup server, create %s owned by %s with mode 0700. Verify its SSH host fingerprint using sudo ssh -p %d %s@%s before the first backup.", d.Directory, d.User, d.Port, d.User, d.Host)
		return nil
	})
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
			return o, err
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
		command := "sh -c " + shellQuote(`set -eu
directory=$1
test -d "$directory"
test "$(realpath -e -- "$directory")" = "$directory"
test "$(stat -c '%u:%a' -- "$directory")" = "$(id -u):700"
test -w "$directory"
printf 'ABR_DESTINATION_OK\n'
`) + " abr-backup " + shellQuote(o.RemoteDir)
		out, err := h.backupSSH(o, command, nil)
		if err != nil {
			return fmt.Errorf("SSH destination check failed; verify credentials, SSH host fingerprint and remote directory ownership/mode: %w", err)
		}
		if strings.TrimSpace(string(out)) != "ABR_DESTINATION_OK" {
			return fmt.Errorf("backup destination verification receipt invalid")
		}
		h.say("Backup destination verified: %s:%s", o.Remote, o.RemoteDir)
		return nil
	})
}
