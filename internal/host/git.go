package host

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"abr/internal/config"
)

// Published by GitHub, not learned from an unauthenticated ssh-keyscan.
const githubHostKey = "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"

func (h Host) gitDir() string { return filepath.Join(h.Manager.StateDir, "git") }

// GitSetup creates or imports one unencrypted VPS identity. Existing keys are
// preserved; rerunning prints the same public key. No GitHub API access is needed.
func (h Host) GitSetup(source string) error {
	return h.locked(func() error {
		dir := h.gitDir()
		key := filepath.Join(dir, "id_ed25519")
		if h.DryRun {
			h.say("Would create/reuse a shared VPS SSH key in %s; no keys generated or imported", dir)
			return nil
		}
		if err := h.gitFile(h.Manager.StateDir, true); err != nil {
			return err
		}
		if err := os.MkdirAll(h.path(dir), 0700); err != nil {
			return err
		}
		if err := h.gitFile(dir, true); err != nil {
			return err
		}
		if _, err := os.Lstat(h.path(key)); os.IsNotExist(err) {
			tmp, err := os.MkdirTemp(h.path(dir), ".key-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			candidate := filepath.Join(tmp, "identity")
			if source == "" {
				if _, err := h.run("Generate shared VPS SSH key", Command{Name: "ssh-keygen", Args: []string{"-t", "ed25519", "-N", "", "-C", "abr VPS", "-f", candidate}, Private: true}); err != nil {
					return err
				}
			} else {
				data, err := readGitKey(source)
				if err != nil {
					return err
				}
				if err := h.write(candidate, data, 0600); err != nil {
					return err
				}
			}
			if _, err := h.publicGitKey(candidate); err != nil {
				return err
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				return err
			}
			if err := h.write(key, data, 0600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := h.gitFile(key, false); err != nil {
			return err
		}
		public, err := h.publicGitKey(h.path(key))
		if err != nil {
			return err
		}
		if source != "" {
			provided, err := h.publicGitKey(source)
			if err != nil {
				return err
			}
			if !bytes.Equal(bytes.TrimSpace(public), bytes.TrimSpace(provided)) {
				return fmt.Errorf("a different shared SSH key already exists; existing key preserved")
			}
		}
		if err := h.write(filepath.Join(dir, "known_hosts"), []byte(githubHostKey), 0600); err != nil {
			return err
		}
		if err := h.write(filepath.Join(dir, "id_ed25519.pub"), public, 0600); err != nil {
			return err
		}
		h.say("Add this public key once in GitHub account Settings → SSH and GPG keys → New SSH key (Authentication):\n%s", strings.TrimSpace(string(public)))
		return nil
	})
}

func (h Host) publicGitKey(path string) ([]byte, error) {
	private, cleanup, err := h.privateGitCopy(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	public, err := h.run("Validate SSH identity (must work without a passphrase)", Command{Name: "ssh-keygen", Args: []string{"-y", "-P", "", "-f", private}, Private: true})
	if err != nil {
		return nil, fmt.Errorf("invalid or passphrase-protected SSH key: %w", err)
	}
	return public, nil
}

// OpenSSH rejects a root-owned identity's ACL mask when root uses it. Root-only
// operations use a temporary 0600 copy; managed users read the single shared key.
func (h Host) privateGitCopy(path string) (string, func(), error) {
	data, err := readGitKey(path)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".git-key-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	key := filepath.Join(dir, "identity")
	if err := h.write(key, data, 0600); err != nil {
		cleanup()
		return "", nil, err
	}
	return key, cleanup, nil
}

func readGitKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("SSH key must be a regular file of at most 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, fmt.Errorf("SSH key is too large")
	}
	return data, nil
}

func (h Host) gitFile(path string, directory bool) error {
	info, err := os.Lstat(h.path(path))
	if err != nil {
		return err
	}
	if info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0027 != 0 {
		return fmt.Errorf("shared Git path must be private and not a symlink: %s", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("shared Git path has an unexpected owner: %s", path)
	}
	return nil
}

func copyEnvironment(environment map[string]string) map[string]string {
	copy := make(map[string]string, len(environment)+2)
	for key, value := range environment {
		copy[key] = value
	}
	return copy
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func (h Host) gitSSH() string {
	return h.gitSSHWithKey(filepath.Join(h.gitDir(), "id_ed25519"))
}

func (h Host) gitSSHWithKey(key string) string {
	dir := h.gitDir()
	return gitSSHCommand(key, filepath.Join(dir, "known_hosts"))
}

func gitSSHCommand(key, knownHosts string) string {
	return "ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=15 -o UserKnownHostsFile=" + shellQuote(knownHosts) + " -i " + shellQuote(key)
}

func (h Host) sharedGit() (bool, error) {
	if _, err := os.Lstat(h.path(h.gitDir())); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	for _, item := range []struct {
		path string
		dir  bool
	}{{h.Manager.StateDir, true}, {h.gitDir(), true}, {filepath.Join(h.gitDir(), "id_ed25519"), false}, {filepath.Join(h.gitDir(), "known_hosts"), false}} {
		if err := h.gitFile(item.path, item.dir); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (h Host) gitAccess(a config.App, revoke bool) error {
	configured, err := h.sharedGit()
	if err != nil || !configured {
		return err
	}
	for _, path := range []string{h.Manager.StateDir, h.gitDir(), filepath.Join(h.gitDir(), "id_ed25519"), filepath.Join(h.gitDir(), "known_hosts")} {
		permission := "r"
		if path == h.Manager.StateDir || path == h.gitDir() {
			permission = "x" // Traverse only; do not expose the state directory listing.
		}
		args := []string{"-m", "u:" + a.User + ":" + permission, "--", path}
		if revoke {
			args[1] = "u:" + a.User + ":---" // Also safe when this user never received access.
		}
		if err := h.command("setfacl", args...); err != nil {
			return err
		}
	}
	return nil
}

var githubRepository = regexp.MustCompile(`^git@github\.com:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\.git$`)

// Clone creates an initial checkout; registration still owns user/DB creation.
func (h Host) Clone(repository, directory string) error {
	if !githubRepository.MatchString(repository) {
		return fmt.Errorf("use a GitHub SSH URL: git@github.com:OWNER/REPO.git")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(h.AppsDir) == "/" || filepath.Dir(filepath.Clean(directory)) != filepath.Clean(h.AppsDir) {
		return fmt.Errorf("clone directory must be directly under %s", h.AppsDir)
	}
	return h.locked(func() (result error) {
		if h.DryRun {
			h.say("Would clone %s into %s using the shared VPS SSH key", repository, directory)
			return nil
		}
		configured, err := h.sharedGit()
		if err != nil {
			return err
		}
		if !configured {
			return fmt.Errorf("run abr git setup and add its public key to GitHub first")
		}
		if _, err := os.Lstat(h.path(directory)); !os.IsNotExist(err) {
			return fmt.Errorf("clone destination must not exist: %s", directory)
		}
		if err := os.MkdirAll(h.path(h.AppsDir), 0755); err != nil {
			return err
		}
		if err := h.trustedDirectory(h.path(h.AppsDir)); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(h.path(h.AppsDir))
		if err != nil || resolved != filepath.Clean(h.path(h.AppsDir)) {
			return fmt.Errorf("apps directory and its parents must not be symlinks")
		}
		info, err := os.Stat(resolved)
		if err != nil || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("apps directory must not be writable by other users")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("apps directory must be owned by the manager's administrator")
		}
		// A fresh clone has no app user yet. Give _apt a temporary identity and
		// staging tree, never access to the live shared identity or root's home.
		entry, exists, err := h.passwd("_apt")
		if err != nil {
			return err
		}
		parts := strings.Split(entry, ":")
		if !exists || len(parts) != 7 || parts[0] != "_apt" || !nonRootIDs(parts[2], parts[3]) {
			return fmt.Errorf("clone requires Ubuntu's unprivileged _apt account")
		}
		stage, err := os.MkdirTemp(h.path(h.AppsDir), ".abr-clone-")
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
		key := filepath.Join(stage, "identity")
		keyData, err := readGitKey(h.path(filepath.Join(h.gitDir(), "id_ed25519")))
		if err != nil {
			return err
		}
		if err := h.write(key, keyData, 0600); err != nil {
			return err
		}
		knownHosts := filepath.Join(stage, "known_hosts")
		if err := h.write(knownHosts, []byte(githubHostKey), 0600); err != nil {
			return err
		}
		if err := h.command("chown", "-hR", "_apt", "--", stage); err != nil {
			return err
		}
		checkout := filepath.Join(stage, "project")
		environment := map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_SSH_COMMAND": gitSSHCommand(key, knownHosts)}
		if _, err := h.unprivileged("_apt", "/nonexistent", "/", environment, true, "git", "-c", "core.hooksPath=/dev/null", "clone", "--template=", "--", repository, checkout); err != nil {
			return err
		}
		if err := h.command("chown", "-hR", "root:root", "--", checkout); err != nil {
			return err
		}
		return os.Rename(checkout, h.path(directory))
	})
}
