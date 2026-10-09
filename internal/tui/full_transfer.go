package tui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func (m *model) fullTransferMenu() tea.Cmd {
	m.menu, m.back = m.fullTransferMenu, m.serverMenu
	var selected string
	return m.setForm("menu", "Full backup & restore", func() tea.Cmd {
		switch selected {
		case "backup":
			return m.fullBackupForm()
		case "restore":
			return m.fullRestoreForm()
		}
		return m.serverMenu()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Backup actions").Description("Preserve app data and settings; restore rebuilds the latest Git code.").Options(
		huh.NewOption("Back up this server", "backup"),
		huh.NewOption("Restore onto a fresh server", "restore"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func archivePath(s string) error {
	if !filepath.IsAbs(s) || !strings.HasSuffix(s, ".tar.gz") || strings.ContainsAny(s, "\x00\r\n") {
		return errors.New("Use an absolute .tar.gz file path")
	}
	return nil
}

func (m *model) fullBackupForm() tea.Cmd {
	output := "/var/backups/abr/full-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	return m.setForm("form", "Full backup", func() tea.Cmd {
		return m.review(action{title: "Full backup", args: []string{"backup", "--output", output}, note: fmt.Sprintf("Archive: %s\n\nPause managed apps and Redis while capturing all managed MySQL databases, Redis data, .env files, Laravel public/private uploads, settings, templates and credentials. Resume previously enabled apps when capture finishes. The private archive contains secrets. Stop any external writers first.", output)})
	}, huh.NewGroup(textInput("Backup archive", "Absolute output path outside app/state directories. Existing files are preserved.", "/var/backups/abr/full.tar.gz", &output).Validate(archivePath)))
}

func (m *model) fullRestoreForm() tea.Cmd {
	var input string
	return m.setForm("form", "Full restore", func() tea.Cmd {
		return m.review(action{title: "Restore onto a fresh server", args: []string{"restore", input, "--yes"}, note: fmt.Sprintf("Archive: %s\n\nConfirm SQL and Redis imports and server provisioning. Requires a fresh Ubuntu server with working SSH key access. Recreate saved app users/accounts, clone the latest Git branches, restore .env/uploads/data, build, migrate and optimize. Previously enabled apps start only after all builds succeed. Keep the old server’s workers stopped during cutover.", input)})
	}, huh.NewGroup(textInput("Backup archive", "Absolute path to the full backup copied onto this fresh server.", "/var/backups/abr/full.tar.gz", &input).Validate(archivePath)))
}
