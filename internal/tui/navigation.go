package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Reload settings when revisiting a menu so saved edits and removals are visible.
func (m *model) appDestination(name string, open func(config.App) tea.Cmd) func() tea.Cmd {
	return func() tea.Cmd {
		c, err := config.Load(m.options.ConfigPath)
		if err != nil {
			cmd := m.home()
			m.notice = clean(err.Error())
			return cmd
		}
		for _, app := range c.Apps {
			if app.Name == name {
				return open(app)
			}
		}
		return m.home()
	}
}

func (m *model) goBack() tea.Cmd {
	back := m.back
	// Clear operation output and callbacks, including explicitly shown secrets.
	m.current, m.next = action{}, nil
	m.output, m.reviewText, m.notice, m.context = "", "", "", ""
	m.result, m.events, m.form = nil, nil, nil
	m.groups = nil
	m.lineLength = 0
	m.viewport.SetContent("")
	if back != nil {
		return back()
	}
	return m.home()
}

func appSummary(app config.App) string {
	summary := app.Name + " · " + app.Type + " · " + app.Domain
	if app.Type == "laravel" {
		web := "FPM"
		if app.Web.Driver == "octane" {
			workers := app.Web.Workers
			if workers == 0 {
				workers = 2
			}
			web = fmt.Sprintf("Octane (%d)", workers)
		}
		summary += fmt.Sprintf("\n  %s · Queue: %d · Scheduler: %s · Nightwatch: %s", web, app.Queue.Workers, onOff(app.Scheduler.Enabled), onOff(app.Nightwatch.Enabled))
	} else {
		summary += "\n  Node"
	}
	return clean(summary)
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (m *model) appDetails(app config.App) tea.Cmd {
	m.back = m.menu
	m.page, m.title, m.form = "details", app.Name+" / Ubuntu user & paths", nil
	m.groups = nil
	m.notice, m.context = "", ""
	lines := []string{
		"Configured Ubuntu user: " + app.User,
		"Managed group: " + app.User,
		"Project directory: " + app.Directory,
	}
	if app.Type == "laravel" {
		lines = append(lines, "Public uploads: "+filepath.Join(app.Directory, "storage/app/public"))
	}
	lines = append(lines, "", "Abr creates the runtime user and matching group during host registration. Use your SSH administrator account for the transfer; the runtime user has no login shell.")
	m.reviewText = clean(strings.Join(lines, "\n"))
	m.viewport.SetContent(ansi.Wrap(m.reviewText, m.bodyWidth(), ""))
	m.viewport.SetHeight(m.bodyHeight())
	m.viewport.GotoTop()
	return nil
}
