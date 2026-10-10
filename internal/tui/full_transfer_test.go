package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestFullTransferFormsReviewAndNavigation(t *testing.T) {
	m := newModel(testOptions(t))
	m.fullTransferMenu()
	m.fullBackupForm()
	if len(m.groups) != 1 || !strings.Contains(m.View().Content, "Example:") {
		t.Fatal("backup form does not have one described field")
	}
	m.next()
	if m.page != "confirm" || m.approved || m.current.args[0] != "backup" || !strings.Contains(m.reviewText, "All services keep running") {
		t.Fatal("backup skipped review")
	}
	m.goBack()
	if m.title != "Full backup" {
		t.Fatal("backup review did not return to input")
	}
	m.fullTransferMenu()
	m.fullRestoreForm()
	path := "/var/backups/abr/full.tar.gz"
	m.Update(tea.PasteMsg{Content: path})
	m.next()
	if m.page != "confirm" || m.approved || strings.Join(m.current.args, " ") != "restore /var/backups/abr/full.tar.gz --yes" || !strings.Contains(m.reviewText, "Confirm SQL and Redis imports") {
		t.Fatal("restore lacks destructive confirmation")
	}
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	if !strings.Contains(m.View().Content, "[ Cancel ]") {
		t.Fatal("restore confirmation must default to cancel")
	}
	for _, args := range [][]string{{"backup"}, {"restore"}} {
		if nextStep(args) == "" {
			t.Fatal("full transfer has no next-step guidance")
		}
	}
	for _, path := range []string{"relative.tar.gz", "/tmp/backup.sql", "/tmp/bad\n.tar.gz"} {
		if archivePath(path) == nil {
			t.Fatal("invalid archive path accepted")
		}
	}
}

func TestOnlineAppBackupReviewAndRestoreDirectory(t *testing.T) {
	m := newModel(testOptions(t))
	m.fullTransferMenu()
	m.appBackupForm()
	if len(m.groups) != 1 || !strings.Contains(m.View().Content, "Example:") {
		t.Fatal("app backup field lacks description/example")
	}
	m.next()
	if m.page != "confirm" || m.approved || strings.Join(m.current.args, " ") != "backup --transfer --all" || !strings.Contains(m.reviewText, "Keep all apps running") {
		t.Fatal("online transfer review inaccurate")
	}
	directory := t.TempDir()
	for _, name := range []string{"api-20261010T020000Z.tar.gz", "portal-20261010T020000Z.tar.gz", "ignored.sql"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := restorePaths(directory)
	if err != nil || len(paths) != 2 {
		t.Fatal("restore directory failed", paths, err)
	}
	if _, err := restorePaths(t.TempDir()); err == nil {
		t.Fatal("empty directory accepted")
	}
	m.backupDestinationForm()
	if !strings.Contains(m.View().Content, "Server IP") || !strings.Contains(m.View().Content, "Example:") {
		t.Fatal("destination configuration missing described first field")
	}
}
