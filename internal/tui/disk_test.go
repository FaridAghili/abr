package tui

import (
	"io"
	"strings"
	"testing"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
)

func TestDiskUsageMenusAndRefreshChoice(t *testing.T) {
	for _, name := range []string{"example", ""} {
		t.Run(name, func(t *testing.T) {
			o := testOptions(t)
			called := make(chan []string, 2)
			o.RunCommand = func(args []string, out io.Writer) error {
				called <- args
				_, err := io.WriteString(out, "disk report\n")
				return err
			}
			m := newModel(o)
			if name == "" {
				m.toolsMenu()
				if !strings.Contains(m.form.View(), "Disk usage for all applications") {
					t.Fatal("all-app disk usage missing from Tools")
				}
				m.diskForm(name)
			} else {
				app := config.App{Name: name}
				m.moreAppMenu(app)
				if !strings.Contains(m.form.View(), "Disk usage") {
					t.Fatal("per-app disk usage missing")
				}
				m.appAction(app, "disk")
			}
			m.next()
			receive(t, m)
			want := strings.TrimSpace("disk " + name)
			if got := strings.Join(<-called, " "); got != want || m.result != nil {
				t.Fatalf("wrong disk report: %s, want %s", got, want)
			}
			if nextStep(m.current.args) == "" {
				t.Fatal("disk report omitted refresh guidance")
			}
			m.diskForm(name)
			press(m, tea.KeyDown)
			m.next()
			receive(t, m)
			if got := strings.Join(<-called, " "); got != want+" --refresh" {
				t.Fatal("refresh selection did not force a new scan")
			}
		})
	}
}
