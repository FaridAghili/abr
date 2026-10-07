package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"abr/internal/config"
)

type purgePlan struct {
	database *credentials
	trees    []string
	files    []string
}

func within(parent, path string) bool {
	parent, path = filepath.Clean(parent), filepath.Clean(path)
	return parent == path || strings.HasPrefix(path, parent+string(filepath.Separator))
}

func (h Host) planPurge(a config.App) (purgePlan, error) {
	p := purgePlan{
		trees: []string{a.Directory, "/var/lib/abr-users/" + a.User, filepath.Join(h.Manager.StateDir, "deployments", a.Name)},
		files: []string{h.credentialsEnvPath(a.Name), filepath.Join(h.Manager.StateDir, "env", a.Name+".env")},
	}
	if !filepath.IsAbs(h.AppsDir) || filepath.Clean(h.AppsDir) == "/" || !within(h.AppsDir, a.Directory) || filepath.Clean(a.Directory) == filepath.Clean(h.AppsDir) {
		return p, fmt.Errorf("project must be in its own directory inside %s", h.AppsDir)
	}
	for _, tree := range p.trees {
		// A changed configuration must never turn an app directory into a
		// shared state/tool directory, or swallow another cleanup target.
		for _, shared := range []string{h.AppsDir, h.Manager.StateDir, h.Manager.ConfigPath, h.TemplatesDir, "/var/lib/abr-users", "/var/lib/caddy", "/etc", "/usr", "/opt"} {
			if within(tree, shared) {
				return p, fmt.Errorf("refusing to delete a shared path inside %s", tree)
			}
		}
		for _, other := range p.trees {
			if tree != other && (within(tree, other) || within(other, tree)) {
				return p, fmt.Errorf("full removal paths overlap")
			}
		}
		if err := h.checkPurgeTree(tree); err != nil {
			return p, err
		}
	}
	if !h.DryRun {
		for _, path := range append(append([]string{}, p.files...), h.userPath(a)) {
			if err := h.trustedFile(h.path(path)); err != nil && !os.IsNotExist(err) {
				return p, err
			}
		}
	}
	data, err := h.read(h.credentialsPath(a.Name))
	if os.IsNotExist(err) {
		return p, nil // No record means no authority to delete a database/account.
	}
	if err != nil {
		return p, err
	}
	var c credentials
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return p, fmt.Errorf("invalid database ownership record for %s", a.Name)
	}
	// Ready may be false after interrupted provisioning. Its recorded database
	// and account still belong to this app, and DROP IF EXISTS is retryable.
	if d.Decode(new(any)) != io.EOF || c.App != a.Name || c.Database != databaseName(a.Name) || c.User != databaseUser(a.Name) || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
		return p, fmt.Errorf("invalid database ownership record for %s", a.Name)
	}
	p.database = &c
	return p, nil
}

func (h Host) checkPurgeTree(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return fmt.Errorf("invalid full removal directory %s", path)
	}
	if h.DryRun {
		return nil
	}
	if err := h.trustedAncestor(filepath.Dir(h.path(path))); err != nil {
		return err
	}
	info, err := os.Lstat(h.path(path))
	if os.IsNotExist(err) {
		return nil // A previous attempt may already have deleted this tree.
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("full removal directory must not be a symlink or file: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(h.path(path))
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(h.path(path)) {
		return fmt.Errorf("full removal directory and its parents must not be symlinks: %s", path)
	}
	if h.root == "" && runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return err
		}
		unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 && within(path, unescape.Replace(fields[4])) {
				return fmt.Errorf("unmount filesystems inside %s before full removal", path)
			}
		}
	}
	return nil
}

func (h Host) purgeData(a config.App, p purgePlan) error {
	if p.database != nil {
		if h.DryRun {
			h.say("Would delete the recorded MySQL database and local accounts for %s", a.Name)
		} else {
			sql := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;\nDROP USER IF EXISTS '%s'@'localhost';\n", p.database.Database, p.database.User)
			if p.database.TCPManaged {
				sql += fmt.Sprintf("DROP USER IF EXISTS '%s'@'127.0.0.1';\n", p.database.User)
			}
			if _, err := h.mysql([]byte(sql)); err != nil {
				return err
			}
		}
	}
	for _, path := range p.trees {
		if err := h.checkPurgeTree(path); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would permanently delete directory %s", path)
		} else if err := os.RemoveAll(h.path(path)); err != nil {
			return err
		}
	}
	for _, path := range p.files {
		if err := h.removeFile(path); err != nil {
			return err
		}
	}
	return nil
}

func (h Host) removePrivateGroup(a config.App, record userRecord) error {
	output, err := h.run("Check private Ubuntu group "+a.User, Command{Name: "getent", Args: []string{"group", a.User}, Private: true})
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return nil // userdel normally removes the private group itself.
	}
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(parts) != 4 || parts[0] != a.User || !nonRootIDs(record.UID, record.GID) || parts[2] != record.GID || parts[3] != "" {
		return fmt.Errorf("refusing to delete changed or shared Ubuntu group %s", a.User)
	}
	users, err := h.run("Verify private Ubuntu group has no remaining users", Command{Name: "getent", Args: []string{"passwd"}, Private: true})
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(users)), "\n") {
		entry := strings.Split(line, ":")
		if len(entry) != 7 || entry[3] == record.GID {
			return fmt.Errorf("refusing to delete Ubuntu group %s with remaining users", a.User)
		}
	}
	return h.command("groupdel", a.User)
}

func (h Host) forgetAppAccess(record userRecord) error {
	paths := map[string]bool{}
	git, err := h.sharedGit()
	if err != nil {
		return err
	}
	if git {
		for _, path := range []string{h.Manager.StateDir, h.gitDir(), filepath.Join(h.gitDir(), "id_ed25519"), filepath.Join(h.gitDir(), "known_hosts")} {
			paths[path] = true
		}
	}
	_, composer, err := h.sharedComposer()
	if err != nil {
		return err
	}
	if composer {
		for _, path := range []string{h.Manager.StateDir, h.composerDir(), filepath.Join(h.composerDir(), "auth.json"), filepath.Join(h.composerDir(), ".htaccess")} {
			paths[path] = true
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if !nonRootIDs(record.UID, record.UID) {
		return fmt.Errorf("invalid managed UID for access cleanup")
	}
	users, err := h.run("Check recorded UID before removing shared credential ACLs", Command{Name: "getent", Args: []string{"passwd"}, Private: true})
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(users)), "\n") {
		if line == "" {
			continue
		}
		entry := strings.Split(line, ":")
		if len(entry) != 7 {
			return fmt.Errorf("invalid Ubuntu account entry during access cleanup")
		}
		if entry[2] == record.UID {
			return nil // A retry must preserve access granted to a reused UID.
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	slices.Sort(ordered)
	for _, path := range ordered {
		// Ensure an entry exists first, so retries also work after an earlier
		// attempt already removed some ACLs. The app's account is gone.
		if err := h.command("setfacl", "-m", "u:"+record.UID+":---", "--", path); err != nil {
			return err
		}
		if err := h.command("setfacl", "-x", "u:"+record.UID, "--", path); err != nil {
			return err
		}
	}
	return nil
}
