package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func (m *model) diskForm(name string) tea.Cmd {
	refresh := false
	title := "Disk usage / " + name
	if name == "" {
		title = "Disk usage / all applications"
	}
	return m.setForm("form", title, func() tea.Cmd {
		args := []string{"disk"}
		if name != "" {
			args = append(args, name)
		}
		if refresh {
			args = append(args, "--refresh")
		}
		return m.start(action{title: title, args: args})
	}, huh.NewGroup(huh.NewSelect[bool]().Title("App size measurements").Description("Recent scans keep repeated views fast; filesystem free space is always live.\nExample: refresh after uploading large files or clearing logs.").Options(
		huh.NewOption("Use recent scan (up to one minute old)", false),
		huh.NewOption("Refresh now", true)).Value(&refresh)))
}
