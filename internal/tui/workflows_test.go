package tui

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func finishStep(t *testing.T, m *model) {
	t.Helper()
	for !receive(t, m).done {
	}
}

func approve(m *model) {
	press(m, 'y')
	press(m, tea.KeyEnter)
}

func TestSetupWorkflowRunsGitAndComposerThenShowsTablePlus(t *testing.T) {
	o := testOptions(t)
	var commands []string
	o.RunCommand = func(args []string, out io.Writer) error {
		commands = append(commands, strings.Join(args, " "))
		if args[0] == "git" {
			_, _ = io.WriteString(out, "ssh-ed25519 fixture-public-key my-vps\n")
		}
		if args[0] == "database" {
			_, _ = io.WriteString(out, "TablePlus login\nDatabase password: fixture-db-password\n")
		}
		return nil
	}
	o.ComposerAuth = func(repository, username, password string, _ io.Writer) error {
		if repository != "nova.laravel.com" || username != "fixture@example.invalid" || password != "fixture-private-token" {
			return errors.New("incorrect Composer credentials")
		}
		commands = append(commands, "composer credentials")
		return nil
	}
	m := newModel(o)
	m.setupForm()
	m.Update(tea.PasteMsg{Content: "my-vps"})
	m.next()
	approve(m)
	finishStep(t, m)
	if len(commands) != 2 || !strings.HasPrefix(commands[0], "setup --hostname my-vps") || commands[1] != "git setup" || !strings.Contains(m.output, "fixture-public-key") || !strings.Contains(m.View().Content, "Enter continue") {
		t.Fatal("setup did not automatically create/display its GitHub key", commands)
	}
	press(m, tea.KeyEnter)
	if !strings.Contains(m.View().Content, "Composer credentials") {
		t.Fatal("setup returned to a menu")
	}
	m.next()
	m.form.NextGroup()
	m.Update(tea.PasteMsg{Content: "fixture@example.invalid"})
	m.form.NextGroup()
	m.Update(tea.PasteMsg{Content: "fixture-private-token"})
	m.next()
	if !m.busy || m.page != "output" {
		t.Fatal("setup asked for a second credential review")
	}
	finishStep(t, m) // Composer → TablePlus command.
	finishStep(t, m)
	if !slices.Equal(commands[2:], []string{"composer credentials", "database --admin --show"}) || !strings.Contains(m.output, "fixture-db-password") || strings.Contains(m.output, "fixture-private-token") {
		t.Fatal("setup did not end with TablePlus credentials", commands)
	}
	press(m, tea.KeyEscape)
	if m.output != "" {
		t.Fatal("setup credentials retained after leaving")
	}
}

func TestSetupCanSkipComposerAndStopsBeforeGitOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		o := testOptions(t)
		var commands []string
		o.RunCommand = func(args []string, _ io.Writer) error {
			commands = append(commands, strings.Join(args, " "))
			if fail {
				return errors.New("setup failed")
			}
			return nil
		}
		m := newModel(o)
		m.setupForm()
		m.next()
		approve(m)
		finishStep(t, m)
		if fail {
			press(m, tea.KeyEnter)
			if len(commands) != 1 || m.page != "home" {
				t.Fatal("failed setup continued")
			}
			continue
		}
		press(m, tea.KeyEnter)
		press(m, tea.KeyDown)
		m.next()
		finishStep(t, m)
		if len(commands) != 3 || commands[2] != "database --admin --show" {
			t.Fatal("skipping Composer did not finish setup")
		}
	}
}

func TestCloneWorkflowCarriesDetectedFrameworkAndNameThroughDeployment(t *testing.T) {
	for _, kind := range []string{"laravel", "nuxt"} {
		t.Run(kind, func(t *testing.T) {
			o := testOptions(t)
			var commands []string
			o.RunCommand = func(args []string, out io.Writer) error {
				commands = append(commands, strings.Join(args, " "))
				if args[0] == "clone" {
					dir := filepath.Join(o.AppsDir, args[2])
					if err := os.MkdirAll(dir, 0755); err != nil {
						return err
					}
					file := "artisan"
					if kind == "nuxt" {
						file = "nuxt.config.ts"
					}
					return os.WriteFile(filepath.Join(dir, file), []byte(""), 0600)
				}
				if args[0] == "env" {
					_, _ = io.WriteString(out, "Prepared .env; set managed MySQL values\n")
				}
				return nil
			}
			m := newModel(o)
			m.cloneForm()
			m.Update(tea.PasteMsg{Content: "git@github.com:owner/app.git"})
			m.form.NextGroup()
			m.Update(tea.PasteMsg{Content: "example-app"})
			m.next()
			approve(m)
			finishStep(t, m)
			if view := m.View().Content; !strings.Contains(view, "Primary domain") || strings.Contains(view, "Application type") || strings.Contains(view, "Application name") || !strings.Contains(m.notice, "Detected "+kind) {
				t.Fatalf("clone did not directly open domain field: %s", view)
			}
			m.Update(tea.PasteMsg{Content: "example.com"})
			m.next()
			args := strings.Join(m.current.args, " ")
			if !strings.Contains(args, "--name example-app --type "+kind+" --domain example.com --canonical-host www") {
				t.Fatal("lost detected registration values", args)
			}
			if kind == "nuxt" && strings.Contains(args, "--web-driver") {
				t.Fatal("Nuxt asked for Laravel settings")
			}
			approve(m)
			finishStep(t, m) // Register → env.
			finishStep(t, m)
			if m.busy || !strings.Contains(m.output, "Prepared .env") || commands[len(commands)-1] != "env example-app" {
				t.Fatal("did not prepare environment")
			}
			press(m, tea.KeyEnter)
			press(m, tea.KeyDown) // Skip editor.
			m.next()
			finishStep(t, m)
			if commands[len(commands)-1] != "deploy example-app --no-pull" || m.result != nil {
				t.Fatal("did not deploy the cloned checkout", commands)
			}
		})
	}
}

func TestUnknownFrameworkKeepsNameAndAsksOnlyForType(t *testing.T) {
	o := testOptions(t)
	if err := os.MkdirAll(filepath.Join(o.AppsDir, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	m := newModel(o)
	m.clonedRegistration("app")
	if !strings.Contains(m.View().Content, "Application type") {
		t.Fatal("unknown framework silently assumed")
	}
	m.form.NextGroup()
	if !strings.Contains(m.View().Content, "Primary domain") {
		t.Fatal("unknown framework asked for name again")
	}
	m.next()
	if !strings.Contains(strings.Join(m.current.args, " "), "--name app") {
		t.Fatal("unknown framework lost clone name")
	}
}

func TestWorkflowFailuresAndPreviewsDoNotAdvance(t *testing.T) {
	for _, stage := range []string{"clone", "register", "env"} {
		for _, preview := range []bool{false, true} {
			t.Run(stage+"/"+map[bool]string{false: "failure", true: "preview"}[preview], func(t *testing.T) {
				o := testOptions(t)
				o.DryRun = preview
				o.RunCommand = func([]string, io.Writer) error {
					if !preview {
						return errors.New("step failed")
					}
					return nil
				}
				m := newModel(o)
				advanced := false
				m.start(action{title: stage, args: []string{stage, "app"}, after: func() tea.Cmd { advanced = true; return nil }, continueWith: func() tea.Cmd { advanced = true; return nil }})
				finishStep(t, m)
				press(m, tea.KeyEnter)
				if advanced || m.page != "home" {
					t.Fatal("failure or preview advanced workflow")
				}
			})
		}
	}
}

func TestEnvironmentEditorSuccessDeploysAndFailureStops(t *testing.T) {
	o := testOptions(t)
	var command string
	o.RunCommand = func(args []string, _ io.Writer) error { command = strings.Join(args, " "); return nil }
	o.EnvEditor = func(name string) (*exec.Cmd, error) {
		if name != "app" {
			t.Fatal("wrong editor app")
		}
		return exec.Command("true"), nil
	}
	m := newModel(o)
	m.environmentChoice("app")
	cmd := m.next()
	if cmd == nil || !m.busy || command != "" {
		t.Fatal("editor did not suspend deployment")
	}
	m.Update(editorFinished{name: "app"})
	finishStep(t, m)
	if command != "deploy app --no-pull" {
		t.Fatal("editor completion did not start deployment")
	}
	command = ""
	m.Update(editorFinished{name: "app", err: errors.New("editor failed")})
	if command != "" || m.busy || m.result == nil {
		t.Fatal("failed editor started deployment")
	}
}
