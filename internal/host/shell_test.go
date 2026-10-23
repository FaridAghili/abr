package host

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type shellRunner struct{ Runner }

func (r shellRunner) Run(c Command) ([]byte, error) {
	data, err := r.Runner.Run(c)
	if err == nil && c.Name == "runuser" {
		return shellUserFixture(c, data)
	}
	if err != nil || c.Name != "git" {
		return data, err
	}
	for _, repo := range shellRepositories {
		if slices.Contains(c.Args, "clone") && slices.Contains(c.Args, repo.url) {
			path := c.Args[len(c.Args)-1]
			if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(filepath.Join(path, repo.entry), []byte("# fixture\n"), 0644)
		}
		if slices.Contains(c.Args, "get-url") && strings.HasSuffix(c.Args[1], repo.path) {
			return []byte(repo.url), nil
		}
	}
	return data, nil
}

// Perform the administrator's unprivileged shell commands on the fixture tree.
func shellUserFixture(c Command, data []byte) ([]byte, error) {
	i := slices.Index(c.Args, "/usr/bin/setpriv")
	if i < 0 || len(c.Args) < i+4 || !slices.Contains(c.Args, "GIT_CONFIG_NOSYSTEM=1") {
		return data, nil
	}
	name, args := c.Args[i+3], c.Args[i+4:]
	at := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(c.Dir, path)
	}
	switch name {
	case "git":
		for _, repo := range shellRepositories {
			if slices.Contains(args, "clone") && slices.Contains(args, repo.url) {
				path := args[len(args)-1]
				if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
					return nil, err
				}
				if repo.path == rootZSH {
					if err := os.MkdirAll(filepath.Join(path, "tools"), 0755); err != nil {
						return nil, err
					}
					if err := os.WriteFile(filepath.Join(path, "tools/upgrade.sh"), []byte("# fixture\n"), 0644); err != nil {
						return nil, err
					}
				}
				return nil, os.WriteFile(filepath.Join(path, repo.entry), []byte("# fixture\n"), 0644)
			}
			if slices.Contains(args, "get-url") && strings.HasSuffix(args[1], strings.TrimPrefix(repo.path, "/root")) {
				return []byte(repo.url), nil
			}
		}
	case "test":
		_, err := os.Stat(args[len(args)-1])
		return nil, err
	case "cat":
		return os.ReadFile(at(args[len(args)-1]))
	case "mktemp":
		f, err := os.CreateTemp(c.Dir, ".abr-zshrc-")
		if err != nil {
			return nil, err
		}
		f.Close()
		return []byte(filepath.Base(f.Name()) + "\n"), nil
	case "tee":
		return nil, os.WriteFile(at(args[len(args)-1]), c.Input, 0600)
	case "chmod":
		return nil, os.Chmod(at(args[len(args)-1]), 0600)
	case "mv":
		return nil, os.Rename(at(args[len(args)-2]), at(args[len(args)-1]))
	case "rm":
		if err := os.Remove(at(args[len(args)-1])); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, nil
}

func prepareShellFixture(t *testing.T, h Host) {
	t.Helper()
	for _, repo := range shellRepositories {
		if err := os.MkdirAll(h.path(filepath.Join(repo.path, ".git")), 0755); err != nil {
			t.Fatal(err)
		}
		if err := h.write(filepath.Join(repo.path, repo.entry), []byte("# fixture\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.write(rootZSH+"/tools/upgrade.sh", []byte("# fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestShellSetupRepeatsPreservesConfigurationAndBackup(t *testing.T) {
	h, runner, _, _ := fixture(t)
	h.Runner = shellRunner{runner}
	old := []byte("ZSH_THEME=agnoster\nplugins=(git docker)\nsource \"$ZSH/oh-my-zsh.sh\"\nalias ll='ls -l'\n")
	if err := h.write("/root/.zshrc", old, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.setupShell("root"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.read("/root/.zshrc")
	if err != nil || strings.Count(string(got), "# Begin abr shell") != 1 || !bytes.Contains(got, []byte("plugins=(git docker)")) || !bytes.Contains(got, []byte("ZSH_THEME=agnoster")) || !bytes.HasSuffix(got, []byte("alias ll='ls -l'\n")) {
		t.Fatalf("configuration not preserved: %s %v", got, err)
	}
	backup, err := h.read("/root/.zshrc.pre-abr")
	if err != nil || !bytes.Equal(backup, old) {
		t.Fatal("original configuration backup changed")
	}
	clones := 0
	for _, c := range runner.calls {
		if c.Name == "git" && slices.Contains(c.Args, "clone") {
			clones++
		}
		if c.Name == "chsh" && !slices.Equal(c.Args, []string{"--shell", "/usr/bin/zsh", "root"}) {
			t.Fatal("changed another user's shell")
		}
	}
	if clones != 3 {
		t.Fatalf("repeated setup cloned %d repositories", clones)
	}
}

func TestShellRejectsUnsafePathsAndFailedValidation(t *testing.T) {
	for _, kind := range []string{"repository symlink", "config symlink", "config syntax", "clone failure"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, _, _ := fixture(t)
			h.Runner = shellRunner{runner}
			if kind != "clone failure" {
				prepareShellFixture(t, h)
			}
			old := []byte("# original config\n")
			if err := h.write("/root/.zshrc", old, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "repository symlink", "config symlink":
				path := h.path("/root/.zshrc")
				if kind == "repository symlink" {
					path = h.path(shellRepositories[1].path)
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(h.path("/outside"), path); err != nil {
					t.Fatal(err)
				}
			default:
				runner.fail = func(c Command) error {
					if c.Name == "zsh" || slices.Contains(c.Args, "clone") {
						return testExit(1)
					}
					return nil
				}
			}
			if err := h.setupShell("root"); err == nil {
				t.Fatal("unsafe or failed setup succeeded")
			}
			if slices.ContainsFunc(runner.calls, func(c Command) bool { return c.Name == "chsh" }) {
				t.Fatal("changed default shell after failure")
			}
			if kind != "config symlink" {
				got, _ := h.read("/root/.zshrc")
				if !bytes.Equal(got, old) {
					t.Fatal("failed setup changed configuration")
				}
			}
		})
	}
}

func TestManagedZSHRCRejectsAmbiguousConfigurationAndRepeats(t *testing.T) {
	block, err := os.ReadFile("../../templates/zshrc.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"", "# existing", "plugins=(\n git\n docker\n)\n. ${ZSH}/oh-my-zsh.sh\n", "source '$HOME/.oh-my-zsh/oh-my-zsh.sh'\n"} {
		got, err := managedZSHRC("/root/.zshrc", []byte(old), block)
		if err != nil {
			t.Fatal(err)
		}
		again, err := managedZSHRC("/root/.zshrc", got, block)
		if err != nil || !bytes.Equal(got, again) {
			t.Fatal("managed block is not repeatable")
		}
	}
	for _, old := range []string{"# Begin abr shell\n", "# End abr shell\n# Begin abr shell\n", "source \"$ZSH/oh-my-zsh.sh\"\nsource \"$ZSH/oh-my-zsh.sh\"\n", "if true; then source $ZSH/oh-my-zsh.sh; fi\n"} {
		if _, err := managedZSHRC("/root/.zshrc", []byte(old), block); err == nil {
			t.Fatalf("ambiguous configuration accepted: %s", old)
		}
	}
}

func TestShellSetupPreviewHasNoSideEffects(t *testing.T) {
	for _, admin := range []string{"root", "farid"} {
		h, runner, out, _ := fixture(t)
		h.DryRun = true
		if err := h.setupShell(admin); err != nil {
			t.Fatal(err)
		}
		if len(runner.calls) != 0 || strings.Contains(out.String(), "default shell is") {
			t.Fatal("preview executed commands or reported completion")
		}
		for _, path := range []string{"/root", "/home", h.Manager.StateDir + "/shell-admin"} {
			if _, err := os.Stat(h.path(path)); !os.IsNotExist(err) {
				t.Fatal("preview created shell files")
			}
		}
	}
}

func shellAdminFixture(t *testing.T) (Host, *fakeRunner, *bytes.Buffer, []byte) {
	t.Helper()
	h, runner, out, _ := fixture(t)
	h.Runner = shellRunner{runner}
	runner.users["farid"] = "farid:x:1000:1000::/home/farid:/bin/bash"
	if err := os.MkdirAll(h.path("/home/farid"), 0755); err != nil {
		t.Fatal(err)
	}
	old := []byte("plugins=(git docker)\nalias ll='ls -l'\n")
	if err := os.WriteFile(h.path("/home/farid/.zshrc"), old, 0644); err != nil {
		t.Fatal(err)
	}
	return h, runner, out, old
}

func TestShellSetupConfiguresAdministratorAsThatUser(t *testing.T) {
	h, runner, out, old := shellAdminFixture(t)
	for range 2 {
		if err := h.setupShell("farid"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(h.path("/home/farid/.zshrc"))
	if err != nil || strings.Count(string(got), "# Begin abr shell") != 1 || !bytes.HasPrefix(got, old) {
		t.Fatalf("administrator configuration not preserved: %s %v", got, err)
	}
	if backup, err := os.ReadFile(h.path("/home/farid/.zshrc.pre-abr")); err != nil || !bytes.Equal(backup, old) {
		t.Fatal("administrator configuration backup missing or changed")
	}
	if record, err := h.read(h.Manager.StateDir + "/shell-admin"); err != nil || string(record) != "farid\n" {
		t.Fatalf("shell administrator not recorded: %q %v", record, err)
	}
	clones, shells := 0, map[string]bool{}
	for _, c := range runner.calls {
		text := strings.Join(c.Args, " ")
		if c.Name == "chsh" {
			shells[c.Args[len(c.Args)-1]] = true
		}
		if c.Name != "runuser" && strings.Contains(text, "/home/farid") {
			t.Fatalf("root operated in the administrator's home: %s %s", c.Name, text)
		}
		if c.Name == "runuser" {
			if !strings.HasPrefix(text, "--user farid -- env -i HOME=") || !slices.Contains(c.Args, "--no-new-privs") {
				t.Fatalf("administrator command not unprivileged: %s", text)
			}
			if slices.Contains(c.Args, "clone") {
				clones++
			}
		}
	}
	if clones != 3 || !shells["root"] || !shells["farid"] || !strings.Contains(out.String(), "Root's and farid's default shell is Zsh") {
		t.Fatalf("administrator shell incomplete: %d clones, %v", clones, shells)
	}
	if _, err := os.Stat(h.path("/home/farid/.oh-my-zsh/custom/plugins/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh")); err != nil {
		t.Fatal(err)
	}
}

func TestShellAdministratorUpdateAndFailures(t *testing.T) {
	h, runner, _, _ := shellAdminFixture(t)
	prepareShellFixture(t, h)
	if err := h.setupShell("farid"); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := h.requireShell(); err != nil {
		t.Fatal(err)
	}
	if err := h.updateShell(); err != nil {
		t.Fatal(err)
	}
	var updates []string
	for _, c := range runner.calls {
		if c.Name == "runuser" && slices.Contains(c.Args, "upgrade.sh") || c.Name == "runuser" && slices.Contains(c.Args, "--ff-only") {
			updates = append(updates, c.Args[len(c.Args)-1])
		}
		if c.Name == "runuser" && slices.Contains(c.Args, "zsh") {
			updates = append(updates, "omz")
		}
	}
	if len(updates) != 3 {
		t.Fatalf("administrator shell not updated: %v", updates)
	}
	if err := os.RemoveAll(h.path("/home/farid/.oh-my-zsh/custom/plugins/zsh-autosuggestions")); err != nil {
		t.Fatal(err)
	}
	if err := h.requireShell(); err == nil {
		t.Fatal("update accepted a missing administrator plugin")
	}

	h, runner, _, _ = shellAdminFixture(t)
	runner.fail = func(c Command) error {
		if c.Name == "zsh" {
			return testExit(1)
		}
		return nil
	}
	if err := h.setupShell("farid"); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if slices.ContainsFunc(runner.calls, func(c Command) bool { return c.Name == "chsh" }) {
		t.Fatal("changed default shell after failure")
	}
	for _, name := range []string{"nobody-here", "../etc"} {
		if err := h.setupShell(name); err == nil {
			t.Fatalf("accepted invalid administrator %s", name)
		}
	}
}

func TestZSHRCTemplateRunsWithExistingPlugins(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("Zsh is not installed; disposable host CI verifies the full installation")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".oh-my-zsh")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oh-my-zsh.sh"), []byte("# local stub; never source the test user's shell configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	block, err := os.ReadFile("../../templates/zshrc.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, initial := range []string{"", "plugins=(docker git zsh-syntax-highlighting zsh-autosuggestions zsh-autosuggestions)\nZSH_THEME=agnoster\n"} {
		script := initial + string(block) + "print -r -- ${(j:,:)plugins}\nprint -r -- $ZSH_THEME\n"
		output, err := (ExecRunner{}).Run(Command{Name: "zsh", Args: []string{"-f"}, Input: []byte(script), Env: []string{"HOME=" + home}})
		want := "git,zsh-autosuggestions,zsh-syntax-highlighting\nrobbyrussell\n"
		if initial != "" {
			want = "git,docker,zsh-autosuggestions,zsh-syntax-highlighting\nagnoster\n"
		}
		if err != nil || string(output) != want {
			t.Fatalf("shell template: %q; want %q: %v", output, want, err)
		}
	}
}
