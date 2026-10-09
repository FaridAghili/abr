package tui

import (
	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func isLogView(args []string) bool {
	return len(args) > 0 && args[0] == "logs" && (len(args) == 1 || args[1] != "clear")
}

func (m *model) allLogsMenu() tea.Cmd {
	m.menu, m.back = m.allLogsMenu, m.toolsMenu
	var selected string
	return m.setForm("menu", "All applications / Logs", func() tea.Cmd {
		switch selected {
		case "read":
			return m.readLogsForm(nil)
		case "clear":
			return m.clearLogsForm("")
		}
		return m.goBack()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Log actions").Description("Read logs together, or clear selected file logs.").Options(
		huh.NewOption("Read logs", "read"),
		huh.NewOption("Clear logs", "clear"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) readLogsForm(app *config.App) tea.Cmd {
	kind := "journal"
	title := "Read logs / all applications"
	if app != nil {
		title = "Read logs / " + app.Name
	}
	return m.setForm("form", title, func() tea.Cmd {
		if kind == "journal" && app != nil {
			return m.serviceForm(*app, "logs")
		}
		args := []string{"logs"}
		if app == nil {
			args = append(args, "--all")
		} else {
			args = append(args, app.Name)
		}
		args = append(args, "--type", kind)
		if app != nil && app.Name == "clear" {
			args = []string{"logs", "--type", kind, "--", app.Name}
		}
		return m.start(action{title: title, args: args, note: "Recent log snapshot · r refresh · Output clears when you leave."})
	}, huh.NewGroup(huh.NewSelect[string]().Title("Which logs?").Description("Choose the log source; all-app file logs are labeled by app and file.\nExample: application files for Laravel's storage/logs/laravel.log.").Options(
		huh.NewOption("Service journals", "journal"),
		huh.NewOption("Application file logs", "application"),
		huh.NewOption("Deployment file logs", "deployment")).Value(&kind)))
}

func (m *model) clearLogsForm(name string) tea.Cmd {
	kind := "all"
	title := "Clear logs / " + name
	if name == "" {
		title = "Clear logs / all applications"
	}
	return m.setForm("form", title, func() tea.Cmd {
		args := []string{"logs", "clear"}
		if name == "" {
			args = append(args, "--all")
		} else {
			args = append(args, name)
		}
		args = append(args, "--type", kind, "--yes")
		return m.review(action{title: title, args: args, note: "Permanently empty the selected *.log files in place. Application logs are in storage/logs (Laravel) and logs; deployment logs are in Abr's per-app deployment directory. Log contents cannot be recovered. Shared service journals, deployment history, uploads and databases are kept."})
	}, huh.NewGroup(huh.NewSelect[string]().Title("Which logs?").Description("Choose which file logs to empty.\nExample: application logs to clear Laravel's laravel.log.").Options(
		huh.NewOption("Application and deployment logs", "all"),
		huh.NewOption("Application logs only", "application"),
		huh.NewOption("Deployment logs only", "deployment")).Value(&kind)))
}
