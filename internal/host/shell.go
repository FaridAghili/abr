package host

import (
	"bytes"
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
	return h.trustedFile(h.path(rootZSH + "/tools/upgrade.sh"))
}

func (h Host) setupShell() error {
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
	if !h.DryRun {
		h.say("Root's default shell is Zsh with Oh My Zsh, autosuggestions and syntax highlighting; reconnect to use it")
	}
	return nil
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
	data, err := managedZSHRC(old, block)
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
func managedZSHRC(old, block []byte) ([]byte, error) {
	const begin, end = "# Begin abr shell", "# End abr shell"
	text := string(old)
	if strings.Count(text, begin) != strings.Count(text, end) || strings.Count(text, begin) > 1 {
		return nil, fmt.Errorf("invalid abr shell block in /root/.zshrc")
	}
	if start := strings.Index(text, begin); start >= 0 {
		finish := strings.Index(text, end)
		if finish < start {
			return nil, fmt.Errorf("invalid abr shell block in /root/.zshrc")
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
			return nil, fmt.Errorf("cannot safely configure the Oh My Zsh source in /root/.zshrc; use a single source \"$ZSH/oh-my-zsh.sh\" line")
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
