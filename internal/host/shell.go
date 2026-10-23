package host

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const rootZSH = "/root/.oh-my-zsh"

var shellRepositories = []struct{ path, url, entry string }{
	{rootZSH, "https://github.com/ohmyzsh/ohmyzsh.git", "oh-my-zsh.sh"},
	{rootZSH + "/custom/plugins/zsh-autosuggestions", "https://github.com/zsh-users/zsh-autosuggestions.git", "zsh-autosuggestions.zsh"},
	{rootZSH + "/custom/plugins/zsh-syntax-highlighting", "https://github.com/zsh-users/zsh-syntax-highlighting.git", "zsh-syntax-highlighting.zsh"},
}

var shellGitEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}

func (h Host) checkShellRepository(path, url, entry string) error {
	for _, dir := range []string{path, filepath.Join(path, ".git")} {
		if err := h.trustedDirectory(h.path(dir)); err != nil {
			return err
		}
	}
	if err := h.trustedFile(h.path(filepath.Join(path, entry))); err != nil {
		return err
	}
	remote, err := h.run("Verify shell repository "+path, Command{Name: "git", Args: []string{"-C", h.path(path), "remote", "get-url", "origin"}, Env: shellGitEnv})
	if err != nil {
		return err
	}
	if strings.TrimSuffix(strings.TrimSpace(string(remote)), ".git") != strings.TrimSuffix(url, ".git") {
		return fmt.Errorf("unexpected shell repository origin: %s", path)
	}
	return nil
}

func (h Host) requireShell() error {
	if h.DryRun {
		return nil
	}
	for _, repo := range shellRepositories {
		if err := h.checkShellRepository(repo.path, repo.url, repo.entry); err != nil {
			return fmt.Errorf("run abr setup before updating the server shell: %w", err)
		}
	}
	if err := h.trustedFile(h.path(rootZSH + "/tools/upgrade.sh")); err != nil {
		return err
	}
	u, ok, err := h.shellAdmin()
	if err != nil || !ok {
		return err
	}
	for _, repo := range shellRepositories {
		if err := h.checkUserShellRepository(u, u.shellPath(repo.path), repo.url, repo.entry); err != nil {
			return fmt.Errorf("run abr setup before updating the server shell: %w", err)
		}
	}
	_, err = h.asShellUser(u, "Verify Oh My Zsh upgrade script for "+u.name, Command{Name: "test", Args: []string{"-f", h.path(u.shellPath(rootZSH + "/tools/upgrade.sh"))}})
	return err
}

// setupShell configures root and, when it is another account, the SSH
// administrator who actually logs in. Root never writes in that user's home.
func (h Host) setupShell(admin string) error {
	for _, repo := range shellRepositories {
		if h.DryRun {
			h.say("Would install/verify %s in %s", repo.url, repo.path)
			continue
		}
		if _, err := os.Lstat(h.path(repo.path)); os.IsNotExist(err) {
			parent := filepath.Dir(h.path(repo.path))
			if err := h.trustedAncestor(parent); err != nil {
				return err
			}
			if err := os.MkdirAll(parent, 0755); err != nil {
				return err
			}
			if _, err := h.run("Install "+repo.url, Command{Name: "git", Args: []string{"clone", "--depth", "1", "--", repo.url, h.path(repo.path)}, Env: shellGitEnv, Stream: true}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := h.checkShellRepository(repo.path, repo.url, repo.entry); err != nil {
			return err
		}
	}
	if err := h.configureZSHRC(); err != nil {
		return err
	}
	if err := h.command("chsh", "--shell", "/usr/bin/zsh", "root"); err != nil {
		return err
	}
	users := "Root's"
	if admin != "root" {
		if err := h.setupUserShell(admin); err != nil {
			return err
		}
		users = "Root's and " + admin + "'s"
	}
	// Update upgrades the administrator recorded by the latest setup.
	if err := h.write(filepath.Join(h.Manager.StateDir, "shell-admin"), []byte(admin+"\n"), 0600); err != nil {
		return err
	}
	if !h.DryRun {
		h.say("%s default shell is Zsh with Oh My Zsh, autosuggestions and syntax highlighting; reconnect to use it", users)
	}
	return nil
}

type shellUser struct{ name, home string }

// shellPath maps a root shell path to the same place in the user's home.
func (u shellUser) shellPath(path string) string {
	return filepath.Join(u.home, strings.TrimPrefix(path, "/root/"))
}

func (h Host) shellAccount(name string) (shellUser, error) {
	if name == "root" || !validAdminUser(name) {
		return shellUser{}, fmt.Errorf("invalid shell administrator username")
	}
	entry, exists, err := h.passwd(name)
	if err != nil {
		return shellUser{}, err
	}
	parts := strings.Split(entry, ":")
	if !exists || len(parts) != 7 || parts[2] == "0" || !filepath.IsAbs(parts[5]) || filepath.Clean(parts[5]) == "/" {
		return shellUser{}, fmt.Errorf("shell administrator %s does not have a valid account/home", name)
	}
	return shellUser{name, filepath.Clean(parts[5])}, nil
}

// shellAdmin returns the non-root administrator recorded by setup, if any.
func (h Host) shellAdmin() (shellUser, bool, error) {
	data, err := h.read(filepath.Join(h.Manager.StateDir, "shell-admin"))
	if os.IsNotExist(err) {
		return shellUser{}, false, nil
	} else if err != nil {
		return shellUser{}, false, err
	}
	name := strings.TrimSpace(string(data))
	if name == "root" {
		return shellUser{}, false, nil
	}
	u, err := h.shellAccount(name)
	return u, err == nil, err
}

// Run as the administrator with a fixed environment, like app commands.
func (h Host) asShellUser(u shellUser, label string, c Command) ([]byte, error) {
	args := []string{"--user", u.name, "--", "env", "-i", "HOME=" + h.path(u.home), "USER=" + u.name, "LOGNAME=" + u.name, "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin"}
	args = append(append(append(args, shellGitEnv...), c.Env...), "/usr/bin/setpriv", "--no-new-privs", "--", c.Name)
	if c.Dir == "" {
		c.Dir = h.path(u.home)
	}
	c.Name, c.Args, c.Env = "runuser", append(args, c.Args...), nil
	return h.run(label+" as "+u.name, c)
}

func (h Host) checkUserShellRepository(u shellUser, path, url, entry string) error {
	if _, err := h.asShellUser(u, "Verify shell repository "+path, Command{Name: "test", Args: []string{"-f", h.path(filepath.Join(path, entry))}}); err != nil {
		return fmt.Errorf("missing shell repository: %s", path)
	}
	remote, err := h.asShellUser(u, "Verify shell repository "+path, Command{Name: "git", Args: []string{"-C", h.path(path), "remote", "get-url", "origin"}})
	if err != nil {
		return err
	}
	if strings.TrimSuffix(strings.TrimSpace(string(remote)), ".git") != strings.TrimSuffix(url, ".git") {
		return fmt.Errorf("unexpected shell repository origin: %s", path)
	}
	return nil
}

func (h Host) setupUserShell(name string) error {
	if h.DryRun {
		h.say("Would install Oh My Zsh and both plugins for %s as that user, configure its ~/.zshrc preserving existing settings, and set its default shell to Zsh", name)
		return nil
	}
	u, err := h.shellAccount(name)
	if err != nil {
		return err
	}
	for _, repo := range shellRepositories {
		path := u.shellPath(repo.path)
		if _, err := os.Lstat(h.path(path)); os.IsNotExist(err) {
			if _, err := h.asShellUser(u, "Install "+repo.url, Command{Name: "git", Args: []string{"clone", "--depth", "1", "--", repo.url, h.path(path)}, Stream: true}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := h.checkUserShellRepository(u, path, repo.url, repo.entry); err != nil {
			return err
		}
	}
	if err := h.configureUserZSHRC(u); err != nil {
		return err
	}
	return h.command("chsh", "--shell", "/usr/bin/zsh", u.name)
}

// Read and write the administrator's files only as that user, through a
// private temporary file and an atomic rename.
func (h Host) configureUserZSHRC(u shellUser) error {
	path := filepath.Join(u.home, ".zshrc")
	var old []byte
	if _, err := os.Lstat(h.path(path)); err == nil {
		old, err = h.asShellUser(u, "Read "+path, Command{Name: "cat", Args: []string{"--", ".zshrc"}, Private: true})
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	block, err := os.ReadFile(filepath.Join(h.TemplatesDir, "zshrc.tmpl"))
	if err != nil {
		return err
	}
	data, err := managedZSHRC(path, old, block)
	if err != nil {
		return err
	}
	if bytes.Equal(data, old) {
		return nil
	}
	if _, err := h.run("Validate "+u.name+"'s Zsh configuration", Command{Name: "zsh", Args: []string{"-f", "-n"}, Input: data, Private: true}); err != nil {
		return err
	}
	if len(old) > 0 {
		if _, err := os.Lstat(h.path(path + ".pre-abr")); os.IsNotExist(err) {
			if err := h.writeShellUserFile(u, ".zshrc.pre-abr", old); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return h.writeShellUserFile(u, ".zshrc", data)
}

func (h Host) writeShellUserFile(u shellUser, name string, data []byte) (result error) {
	output, err := h.asShellUser(u, "Create private shell file", Command{Name: "mktemp", Args: []string{"--", ".abr-zshrc-XXXXXXXXXX"}, Private: true})
	if err != nil {
		return err
	}
	temp := strings.TrimSpace(string(output))
	if filepath.Base(temp) != temp || !strings.HasPrefix(temp, ".abr-zshrc-") || strings.ContainsAny(temp, "\x00\r\n") {
		return fmt.Errorf("invalid temporary shell filename")
	}
	defer func() {
		_, err := h.asShellUser(u, "Remove temporary shell file", Command{Name: "rm", Args: []string{"-f", "--", temp}, Private: true})
		result = errors.Join(result, err)
	}()
	if _, err := h.asShellUser(u, "Write "+name, Command{Name: "tee", Args: []string{"--", temp}, Input: data, Private: true}); err != nil {
		return err
	}
	if _, err := h.asShellUser(u, "Protect "+name, Command{Name: "chmod", Args: []string{"600", "--", temp}, Private: true}); err != nil {
		return err
	}
	_, err = h.asShellUser(u, "Install "+name, Command{Name: "mv", Args: []string{"-fT", "--", temp, name}, Private: true})
	return err
}

func (h Host) updateShell() error {
	if _, err := h.run("Update Oh My Zsh for root", Command{
		Name: "zsh", Args: []string{"-f", h.path(rootZSH + "/tools/upgrade.sh"), "-v", "minimal"}, Dir: h.path(rootZSH),
		Env: append(append([]string{}, shellGitEnv...), "ZSH="+h.path(rootZSH)), Stream: true,
	}); err != nil {
		return err
	}
	for _, repo := range shellRepositories[1:] {
		if _, err := h.run("Update "+filepath.Base(repo.path), Command{Name: "git", Args: []string{"-C", h.path(repo.path), "pull", "--ff-only"}, Env: shellGitEnv, Stream: true}); err != nil {
			return err
		}
	}
	if h.DryRun {
		if data, err := h.read(filepath.Join(h.Manager.StateDir, "shell-admin")); err == nil && strings.TrimSpace(string(data)) != "root" {
			h.say("Would update Oh My Zsh and both plugins for %s as that user", strings.TrimSpace(string(data)))
		}
		return nil
	}
	u, ok, err := h.shellAdmin()
	if err != nil || !ok {
		return err
	}
	omz := h.path(u.shellPath(rootZSH))
	if _, err := h.asShellUser(u, "Update Oh My Zsh", Command{Name: "zsh", Args: []string{"-f", omz + "/tools/upgrade.sh", "-v", "minimal"}, Dir: omz, Env: []string{"ZSH=" + omz}, Stream: true}); err != nil {
		return err
	}
	for _, repo := range shellRepositories[1:] {
		if _, err := h.asShellUser(u, "Update "+filepath.Base(repo.path), Command{Name: "git", Args: []string{"-C", h.path(u.shellPath(repo.path)), "pull", "--ff-only"}, Stream: true}); err != nil {
			return err
		}
	}
	return nil
}

func (h Host) configureZSHRC() error {
	if h.DryRun {
		h.say("Would configure /root/.zshrc with Oh My Zsh and both plugins, preserving existing settings")
		return nil
	}
	const path = "/root/.zshrc"
	var old []byte
	if err := h.trustedFile(h.path(path)); err == nil {
		var err error
		old, err = h.read(path)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	block, err := os.ReadFile(filepath.Join(h.TemplatesDir, "zshrc.tmpl"))
	if err != nil {
		return err
	}
	data, err := managedZSHRC(path, old, block)
	if err != nil {
		return err
	}
	if bytes.Equal(data, old) {
		return nil
	}
	if _, err := h.run("Validate root's Zsh configuration", Command{Name: "zsh", Args: []string{"-f", "-n"}, Input: data, Private: true}); err != nil {
		return err
	}
	if len(old) > 0 {
		backup := path + ".pre-abr"
		if _, err := os.Lstat(h.path(backup)); os.IsNotExist(err) {
			if err := h.write(backup, old, 0600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return h.write(path, data, 0600)
}

var omzSource = regexp.MustCompile(`^\s*(source|\.)\s+["']?\$(ZSH|\{ZSH\}|HOME/\.oh-my-zsh|\{HOME\}/\.oh-my-zsh)/oh-my-zsh\.sh["']?\s*(#.*)?$`)

// Insert at the existing OMZ source so plugins and theme settings still take
// effect. Shell code is preserved verbatim and never evaluated during setup.
func managedZSHRC(path string, old, block []byte) ([]byte, error) {
	const begin, end = "# Begin abr shell", "# End abr shell"
	text := string(old)
	if strings.Count(text, begin) != strings.Count(text, end) || strings.Count(text, begin) > 1 {
		return nil, fmt.Errorf("invalid abr shell block in %s", path)
	}
	if start := strings.Index(text, begin); start >= 0 {
		finish := strings.Index(text, end)
		if finish < start {
			return nil, fmt.Errorf("invalid abr shell block in %s", path)
		}
		finish += len(end)
		if finish < len(text) && text[finish] == '\n' {
			finish++
		}
		return []byte(text[:start] + string(block) + text[finish:]), nil
	}
	lines := strings.SplitAfter(text, "\n")
	source := -1
	for i, line := range lines {
		active := strings.TrimSpace(line)
		if strings.HasPrefix(active, "#") || !strings.Contains(active, "oh-my-zsh.sh") {
			continue
		}
		if source >= 0 || !omzSource.MatchString(strings.TrimSuffix(line, "\n")) {
			return nil, fmt.Errorf("cannot safely configure the Oh My Zsh source in %s; use a single source \"$ZSH/oh-my-zsh.sh\" line", path)
		}
		source = i
	}
	if source >= 0 {
		lines[source] = string(block)
		return []byte(strings.Join(lines, "")), nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return []byte(text + string(block)), nil
}
