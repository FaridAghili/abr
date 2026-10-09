package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"abr/internal/config"
)

type composerLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type composerAuth struct {
	HTTPBasic map[string]composerLogin `json:"http-basic"`
}

var composerRepository = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func (h Host) composerDir() string { return filepath.Join(h.Manager.StateDir, "composer") }

// ComposerAuth saves HTTP Basic credentials once for every managed app's deploys.
// Secrets enter through memory/stdin, never subprocess arguments or output.
func (h Host) ComposerAuth(repository, username, password string) error {
	if !composerRepository.MatchString(repository) || strings.Contains(repository, "..") {
		return fmt.Errorf("Composer repository must be a lowercase hostname without a URL or path")
	}
	return h.locked(func() error {
		if h.DryRun {
			h.say("Would save shared Composer credentials for %s in %s/auth.json", repository, h.composerDir())
			return nil
		}
		if strings.TrimSpace(username) == "" || password == "" || len(username)+len(password) > 4096 || strings.ContainsAny(username+password, "\x00\r\n") {
			return fmt.Errorf("Composer username and token must be nonempty single-line values (max 4096 bytes combined)")
		}
		auth, configured, err := h.sharedComposer()
		if err != nil {
			return err
		}
		if !configured {
			auth.HTTPBasic = map[string]composerLogin{}
		}
		auth.HTTPBasic[repository] = composerLogin{Username: username, Password: password}
		data, err := json.MarshalIndent(auth, "", "  ")
		if err != nil || len(data) > 64<<10 {
			return fmt.Errorf("shared Composer credentials exceed 64 KiB")
		}
		if err := h.write(filepath.Join(h.composerDir(), "auth.json"), data, 0600); err != nil {
			return err
		}
		// Composer's home is root-owned/read-only to apps. Caches stay per user.
		if err := h.writeComposerGuard(); err != nil {
			return err
		}
		h.say("Saved shared Composer credentials for %s; future deployments reuse them", repository)
		return nil
	})
}

func (h Host) writeComposerGuard() error {
	return h.write(filepath.Join(h.composerDir(), ".htaccess"), []byte("Deny from all\n"), 0600)
}

func (h Host) composerPath(path string, directory bool) error {
	info, err := os.Lstat(h.path(path))
	if err != nil {
		return err
	}
	if info.IsDir() != directory || info.Mode().Perm()&0027 != 0 {
		return fmt.Errorf("shared Composer path must be private: %s", path)
	}
	if directory {
		return h.trustedDirectory(h.path(path))
	}
	return h.trustedFile(h.path(path))
}

func (h Host) sharedComposer() (composerAuth, bool, error) {
	var auth composerAuth
	if _, err := os.Lstat(h.path(h.composerDir())); os.IsNotExist(err) {
		return auth, false, nil
	} else if err != nil {
		return auth, false, err
	}
	for _, dir := range []string{h.Manager.StateDir, h.composerDir()} {
		if err := h.composerPath(dir, true); err != nil {
			return auth, false, err
		}
	}
	path := filepath.Join(h.composerDir(), "auth.json")
	if err := h.composerPath(path, false); err != nil {
		return auth, false, err
	}
	info, err := os.Stat(h.path(path))
	if err != nil || info.Size() > 64<<10 {
		return auth, false, fmt.Errorf("cannot read shared Composer credentials (max 64 KiB)")
	}
	data, err := h.read(path)
	if err != nil {
		return auth, false, err
	}
	if json.Unmarshal(data, &auth) != nil || len(auth.HTTPBasic) == 0 {
		return auth, false, fmt.Errorf("invalid shared Composer credentials")
	}
	for repository, login := range auth.HTTPBasic {
		if !composerRepository.MatchString(repository) || strings.TrimSpace(login.Username) == "" || login.Password == "" {
			return composerAuth{}, false, fmt.Errorf("invalid shared Composer credentials")
		}
	}
	return auth, true, nil
}

func (h Host) composerAccess(a config.App, revoke bool) error {
	_, configured, err := h.sharedComposer()
	if err != nil || !configured {
		return err
	}
	for _, path := range []string{h.Manager.StateDir, h.composerDir(), filepath.Join(h.composerDir(), "auth.json"), filepath.Join(h.composerDir(), ".htaccess")} {
		permission := "r"
		if path == h.Manager.StateDir || path == h.composerDir() {
			permission = "x"
		}
		if revoke {
			permission = "---"
		}
		if err := h.command("setfacl", "-m", "u:"+a.User+":"+permission, "--", path); err != nil {
			return err
		}
	}
	return nil
}
