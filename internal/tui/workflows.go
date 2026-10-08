package tui

import (
	"fmt"
	"io"
	"path/filepath"

	"abr/internal/project"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func (m *model) setupRunner(args []string) func(io.Writer) error {
	run := m.options.RunCommand
	return func(out io.Writer) error {
		if err := run(args, out); err != nil {
			return err
		}
		return run([]string{"git", "setup"}, out)
	}
}

func (m *model) setupComposerChoice() tea.Cmd {
	var save = true
	return m.setForm("form", "VPS setup / Composer", func() tea.Cmd {
		if save {
			return m.composerCredentialsForm(true)
		}
		return m.setupComplete()
	}, huh.NewGroup(huh.NewSelect[bool]().Title("Composer credentials").Description("Add the printed SSH key to GitHub. Save private-package credentials next, or skip if unnecessary.\nExample: your Laravel Nova account.").Options(huh.NewOption("Configure credentials", true), huh.NewOption("Skip · no private packages", false)).Value(&save)))
}

func (m *model) setupComplete() tea.Cmd {
	return m.start(action{title: "VPS ready / TablePlus MySQL login", args: []string{"database", "--admin", "--show"}})
}

func (m *model) clonedRegistration(name string) tea.Cmd {
	kind, err := project.Detect(filepath.Join(m.options.AppsDir, name))
	if err != nil {
		return m.workflowError("Detect framework: "+name, err)
	}
	cmd := m.registrationForm(name, kind, true)
	if kind != "" {
		m.notice = "Detected " + kind + " · " + name
	} else {
		m.notice = "Framework is unknown or ambiguous; choose Laravel or Nuxt."
	}
	return cmd
}

func (m *model) prepareEnvironment(name string) tea.Cmd {
	exists, err := project.HasEnvExample(filepath.Join(m.options.AppsDir, name))
	if err != nil {
		return m.workflowError("Check .env.example: "+name, err)
	}
	if !exists {
		return m.skipEnvironment(name)
	}
	return m.start(action{title: "Prepare .env: " + name, args: []string{"env", name}, continueWith: func() tea.Cmd { return m.environmentChoice(name) }})
}

func (m *model) environmentChoice(name string) tea.Cmd {
	exists, err := project.HasEnvExample(filepath.Join(m.options.AppsDir, name))
	if err != nil {
		return m.workflowError("Check .env.example: "+name, err)
	}
	if !exists {
		return m.skipEnvironment(name)
	}
	edit := true
	return m.setForm("form", name+" / Environment", func() tea.Cmd {
		if edit {
			if m.options.EnvEditor == nil {
				return m.workflowError("Edit .env: "+name, fmt.Errorf("environment editor is unavailable"))
			}
			command, err := m.options.EnvEditor(name)
			if err != nil {
				return m.workflowError("Edit .env: "+name, err)
			}
			if command == nil {
				return m.skipEnvironment(name)
			}
			m.page, m.title, m.form, m.busy = "output", "Edit .env: "+name, nil, true
			m.next, m.current = nil, action{}
			return tea.ExecProcess(command, func(err error) tea.Msg { return editorFinished{name: name, err: err} })
		}
		return m.firstDeploy(name)
	}, huh.NewGroup(huh.NewSelect[bool]().Title("Edit .env before deployment?").Description("Review app secrets in nano. Save with Ctrl+O, Enter; close with Ctrl+X to deploy automatically.\nExample: add mail or API credentials.").Options(huh.NewOption("Open .env in editor", true), huh.NewOption("Deploy with prepared .env", false)).Value(&edit)))
}

func (m *model) skipEnvironment(name string) tea.Cmd {
	cmd := m.firstDeploy(name)
	m.appendOutput("No .env.example; skipped environment preparation and editor.\n")
	return cmd
}

type editorFinished struct {
	name string
	err  error
}

func (m *model) firstDeploy(name string) tea.Cmd {
	return m.start(action{title: "Deploy " + name, args: []string{"deploy", name, "--no-pull"}})
}

func (m *model) workflowError(title string, err error) tea.Cmd {
	m.back = m.menu
	m.page, m.title, m.form, m.busy, m.result = "output", title, nil, false, err
	m.groups = nil
	m.current, m.next = action{}, nil
	m.notice, m.context = "", ""
	m.output, m.lineLength = "", 0
	m.appendOutput("Error: " + clean(err.Error()) + "\n")
	return nil
}
