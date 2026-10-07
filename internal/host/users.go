package host

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/services"
)

type userRecord struct{ App, User, UID, Home, GID string }

func RuntimeUser(name string) string {
	if len(name) <= 26 {
		return "abr-" + name
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("abr-%s-%x", name[:17], sum[:4])
}

func (h Host) userPath(a config.App) string {
	return filepath.Join(h.Manager.StateDir, "users", a.User+".json")
}

func (h Host) project(a config.App) error {
	base := filepath.Clean(h.AppsDir)
	if !filepath.IsAbs(base) || base == "/" {
		return fmt.Errorf("apps directory must be an absolute dedicated directory")
	}
	rel, err := filepath.Rel(base, a.Directory)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: project must be inside %s, in its own directory", a.Name, base)
	}
	if h.DryRun {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(h.path(a.Directory))
	if err != nil {
		return fmt.Errorf("clone %s first: %w", a.Directory, err)
	}
	if resolved != filepath.Clean(h.path(a.Directory)) {
		return fmt.Errorf("project directory and its parents must not be symlinks")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("project is not a directory")
	}
	// App users own their project, but must never be able to rename its parent
	// while root is changing ownership or permissions beneath it.
	return h.trustedDirectory(filepath.Dir(h.path(a.Directory)))
}

// A private path under a sticky administrator-owned /tmp is safe for tests and
// backups. Other writable or foreign-owned ancestors allow path replacement.
func (h Host) trustedDirectory(path string) error {
	first := true
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || int(stat.Uid) != os.Geteuid() || (info.Mode().Perm()&0022 != 0 && (first || info.Mode()&os.ModeSticky == 0)) {
			return fmt.Errorf("directory must have trusted ownership and permissions, without symlinks: %s", path)
		}
		parent := filepath.Dir(path)
		if parent == path || path == h.root {
			return nil
		}
		path = parent
		first = false
	}
}

func (h Host) trustedAncestor(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return h.trustedDirectory(path)
		}
		path = filepath.Dir(path)
	}
}

func (h Host) trustedFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("managed file must have trusted ownership and permissions, without symlinks: %s", path)
	}
	return nil
}

func (h Host) passwd(user string) (string, bool, error) {
	output, err := h.run("Check Ubuntu account "+user, Command{Name: "getent", Args: []string{"passwd", user}, Private: true})
	if err != nil {
		// getent exit 2 specifically means no matching entry.
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(output)), true, nil
}

func (h Host) ensureUser(a config.App) error {
	if h.DryRun {
		h.say("Would create/verify managed Ubuntu user %s and own %s", a.User, a.Directory)
		return nil
	}
	record := userRecord{App: a.Name, User: a.User, Home: "/var/lib/abr-users/" + a.User}
	saved, err := h.read(h.userPath(a))
	if err == nil {
		if err := json.Unmarshal(saved, &record); err != nil {
			return fmt.Errorf("corrupt managed user record: %w", err)
		}
		if record.App != a.Name || record.User != a.User || record.Home != "/var/lib/abr-users/"+a.User {
			return fmt.Errorf("Ubuntu user %s is managed for another application", a.User)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	entry, exists, err := h.passwd(a.User)
	if err != nil {
		return err
	}
	if exists {
		parts := strings.Split(entry, ":")
		if saved == nil || !validRuntimeAccount(parts, a.User) || parts[4] != "abr-"+a.Name || parts[5] != record.Home || (record.UID != "" && record.UID != parts[2]) {
			return fmt.Errorf("refusing to adopt existing Ubuntu user %s; choose a new dedicated user", a.User)
		}
		if record.UID == "" {
			if err := h.prepareHome(a.User, record.Home); err != nil {
				return err
			}
		}
		return h.saveUser(a, record, parts[2])
	}
	// Record intent first, so interruption after useradd can be recovered by identity.
	data, _ := json.Marshal(record)
	if err := h.write(h.userPath(a), data, 0600); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path("/var/lib/abr-users"), 0755); err != nil {
		return err
	}
	homeOption := "--create-home"
	if info, err := os.Lstat(h.path(record.Home)); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("managed home must be a directory without symlinks: %s", record.Home)
		}
		homeOption = "--no-create-home"
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := h.command("useradd", "--system", "--user-group", homeOption, "--home-dir", record.Home, "--shell", "/usr/sbin/nologin", "--comment", "abr-"+a.Name, a.User); err != nil {
		return err
	}
	entry, exists, err = h.passwd(a.User)
	if err != nil {
		return err
	}
	parts := strings.Split(entry, ":")
	if !exists || !validRuntimeAccount(parts, a.User) || parts[4] != "abr-"+a.Name || parts[5] != record.Home {
		return fmt.Errorf("could not verify newly created user %s", a.User)
	}
	if err := h.prepareHome(a.User, record.Home); err != nil {
		return err
	}
	return h.saveUser(a, record, parts[2])
}

func validRuntimeAccount(parts []string, user string) bool {
	if len(parts) != 7 || parts[0] != user || parts[6] != "/usr/sbin/nologin" {
		return false
	}
	return nonRootIDs(parts[2], parts[3])
}

func nonRootIDs(uidText, gidText string) bool {
	uid, uidErr := strconv.ParseUint(uidText, 10, 32)
	gid, gidErr := strconv.ParseUint(gidText, 10, 32)
	return uidErr == nil && gidErr == nil && uid > 0 && gid > 0
}

func (h Host) prepareHome(user, home string) error {
	// Removal retains the home as root-owned data. Re-registration (including
	// retry after interrupted useradd) must restore access for the new UID.
	if err := h.trustedDirectory(filepath.Dir(h.path(home))); err != nil {
		return err
	}
	info, err := os.Lstat(h.path(home))
	if err != nil || !info.IsDir() {
		return fmt.Errorf("managed home must be a directory without symlinks: %s", home)
	}
	if err := h.command("chown", "-hR", user+":"+user, "--", home); err != nil {
		return err
	}
	return h.command("chmod", "700", "--", home)
}

func (h Host) saveUser(a config.App, record userRecord, uid string) error {
	value, err := strconv.Atoi(uid)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid managed UID")
	}
	record.UID = uid
	data, _ := json.Marshal(record)
	return h.write(h.userPath(a), data, 0600)
}

func (h Host) permissions(a config.App) error {
	if err := h.project(a); err != nil {
		return err
	}
	if !h.DryRun && a.Type == "laravel" {
		for _, suffix := range []string{"public", "storage", "storage/app", "storage/app/public"} {
			path := h.path(filepath.Join(a.Directory, suffix))
			resolved, err := filepath.EvalSymlinks(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if resolved != filepath.Clean(path) {
				return fmt.Errorf("managed asset/storage directory must not be a symlink: %s", suffix)
			}
		}
	}
	if err := h.command("chown", "-hR", a.User+":"+a.User, "--", a.Directory); err != nil {
		return err
	}
	if err := h.command("chmod", "750", "--", a.Directory); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Would secure a regular project .env to mode 0600")
	} else {
		// Use a file descriptor and refuse symlinks before changing secret permissions.
		f, err := os.OpenFile(h.path(filepath.Join(a.Directory, ".env")), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err == nil {
			info, statErr := f.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				f.Close()
				return fmt.Errorf("project .env must be a regular file")
			}
			modeErr := f.Chmod(0600)
			f.Close()
			if modeErr != nil {
				return modeErr
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("project .env must be a readable regular file: %w", err)
		}
	}
	if err := h.command("setfacl", "-m", "u:caddy:--x", "--", a.Directory); err != nil {
		return err
	}
	if a.Type == "laravel" {
		for _, suffix := range []string{"public", "storage/app/public"} {
			dir := filepath.Join(a.Directory, suffix)
			if !h.DryRun {
				resolved, err := filepath.EvalSymlinks(h.path(dir))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return err
				}
				if resolved != filepath.Clean(h.path(dir)) {
					return fmt.Errorf("public asset directory must not be a symlink: %s", dir)
				}
			}
			if _, err := h.asUser(a, nil, false, "setfacl", "-R", "-P", "-m", "u:caddy:rX", "--", dir); err != nil {
				return err
			}
			if _, err := h.asUser(a, nil, false, "find", "-P", dir, "-type", "d", "-exec", "setfacl", "-m", "d:u:caddy:rx", "--", "{}", "+"); err != nil {
				return err
			}
		}
		// Caddy needs traversal to Laravel's public storage symlink target.
		for _, suffix := range []string{"storage", "storage/app"} {
			if _, err := h.asUser(a, nil, false, "setfacl", "-m", "u:caddy:--x", "--", filepath.Join(a.Directory, suffix)); err != nil {
				return err
			}
		}
	}
	if a.Type == "nuxt" {
		for _, suffix := range []string{".output", ".output/public"} {
			dir := filepath.Join(a.Directory, suffix)
			if !h.DryRun {
				resolved, err := filepath.EvalSymlinks(h.path(dir))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return err
				}
				if resolved != filepath.Clean(h.path(dir)) {
					return fmt.Errorf("Nuxt output directory must not be a symlink: %s", dir)
				}
			}
			acl := "u:caddy:--x"
			flags := []string{"-m", acl, "--", dir}
			if suffix == ".output/public" {
				flags = []string{"-R", "-P", "-m", "u:caddy:rX", "--", dir}
			}
			if _, err := h.asUser(a, nil, false, "setfacl", flags...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Host) Register(a config.App, imports map[string]int) (ports.Registry, error) {
	var registry ports.Registry
	err := h.locked(func() error {
		var err error
		registry, err = h.Manager.PreviewRegister(a, imports)
		if err != nil {
			return err
		}
		if _, err := services.Render(a, registry, h.TemplatesDir, h.Manager.StateDir); err != nil {
			return err
		}
		if err := h.project(a); err != nil {
			return err
		}
		if err := h.ensureUser(a); err != nil {
			return err
		}
		if !h.DryRun {
			registry, err = h.Manager.Register(a, imports)
			if err != nil {
				return err
			}
		}
		if err := h.permissions(a); err != nil {
			return err
		}
		if a.Database.Enabled {
			if err := h.database(a, false); err != nil {
				return err
			}
		}
		if h.DryRun {
			h.say("Would save configuration and port reservations for %s", a.Name)
		} else {
			h.say("Registered %s with managed user %s", a.Name, a.User)
		}
		return nil
	})
	return registry, err
}
