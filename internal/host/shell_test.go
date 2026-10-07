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
		if err := h.setupShell(); err != nil {
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
			if err := h.setupShell(); err == nil {
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
		got, err := managedZSHRC([]byte(old), block)
		if err != nil {
			t.Fatal(err)
		}
		again, err := managedZSHRC(got, block)
		if err != nil || !bytes.Equal(got, again) {
			t.Fatal("managed block is not repeatable")
		}
	}
	for _, old := range []string{"# Begin abr shell\n", "# End abr shell\n# Begin abr shell\n", "source \"$ZSH/oh-my-zsh.sh\"\nsource \"$ZSH/oh-my-zsh.sh\"\n", "if true; then source $ZSH/oh-my-zsh.sh; fi\n"} {
		if _, err := managedZSHRC([]byte(old), block); err == nil {
			t.Fatalf("ambiguous configuration accepted: %s", old)
		}
	}
}

func TestShellSetupPreviewHasNoSideEffects(t *testing.T) {
	h, runner, out, _ := fixture(t)
	h.DryRun = true
	if err := h.setupShell(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || strings.Contains(out.String(), "Root's default shell is") {
		t.Fatal("preview executed commands or reported completion")
	}
	if _, err := os.Stat(h.path("/root")); !os.IsNotExist(err) {
		t.Fatal("preview created shell files")
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
