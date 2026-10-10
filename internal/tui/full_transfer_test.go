package tui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/backup"

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

func TestBackupHostIdentityWorkflow(t *testing.T) {
	for _, scenario := range []string{"unknown", "known", "cancel", "scan-failure", "trust-failure", "preview"} {
		t.Run(scenario, func(t *testing.T) {
			o := testOptions(t)
			o.DryRun = scenario == "preview"
			key := backup.HostKey{Address: "[192.0.2.10]:2222", Fingerprint: "SHA256:fixture-fingerprint", Known: scenario == "known"}
			var calls []string
			o.BackupHostKey = func(io.Writer) (backup.HostKey, error) {
				calls = append(calls, "inspect")
				if scenario == "scan-failure" {
					return key, errors.New("host key changed")
				}
				return key, nil
			}
			o.BackupTrust = func(fingerprint string, _ io.Writer) error {
				if fingerprint != key.Fingerprint {
					t.Error("trusted an unreviewed fingerprint")
				}
				calls = append(calls, "trust")
				if scenario == "trust-failure" {
					return errors.New("fingerprint changed before confirmation")
				}
				return nil
			}
			o.RunCommand = func(args []string, _ io.Writer) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			m := newModel(o)
			m.fullTransferMenu()
			m.backupTestReview()
			approve(m)
			finishStep(t, m)
			if scenario == "scan-failure" || scenario == "preview" {
				if len(calls) != 1 || m.page != "output" {
					t.Fatal("failure/preview proceeded to trust or test", calls)
				}
				return
			}
			if scenario == "known" {
				finishStep(t, m)
				if strings.Join(calls, ",") != "inspect,backup test" {
					t.Fatal("known host was prompted/trusted again", calls)
				}
				return
			}
			if m.page != "confirm" || m.approved || !strings.Contains(m.reviewText, key.Address) || !strings.Contains(m.reviewText, key.Fingerprint) || !strings.Contains(m.reviewText, "ssh-keygen -lf") || len(calls) != 1 {
				t.Fatal("unknown host bypassed fingerprint confirmation", calls, m.reviewText)
			}
			if scenario == "cancel" {
				press(m, tea.KeyEnter)
				if len(calls) != 1 || m.busy {
					t.Fatal("cancel trusted or tested host", calls)
				}
				return
			}
			approve(m)
			finishStep(t, m)
			want := "inspect,trust,backup test"
			if scenario == "trust-failure" {
				want = "inspect,trust"
			}
			if strings.Join(calls, ",") != want {
				t.Fatal("test ran before successful explicit trust", calls)
			}
		})
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
