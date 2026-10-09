package tui

import (
	"fmt"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
)

type artisanShellFinished struct {
	name string
	err  error
}

func (m *model) artisanShell(app config.App) tea.Cmd {
	if app.Type != "laravel" {
		return m.workflowError("Artisan shell", fmt.Errorf("Artisan access is only available for Laravel apps"))
	}
	if m.options.DryRun {
		return m.workflowError("Artisan shell", fmt.Errorf("interactive shell is unavailable in preview mode"))
	}
	if m.options.ArtisanShell == nil {
		return m.workflowError("Artisan shell", fmt.Errorf("Artisan shell is unavailable"))
	}
	command, err := m.options.ArtisanShell(app.Name)
	if err != nil {
		return m.workflowError("Artisan shell: "+app.Name, err)
	}
	if command == nil {
		return m.workflowError("Artisan shell", fmt.Errorf("no shell process was prepared"))
	}
	m.back = m.menu
	m.page, m.title, m.form, m.busy = "output", "Artisan shell: "+app.Name, nil, true
	m.next, m.current = nil, action{}
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return artisanShellFinished{name: app.Name, err: err}
	})
}
