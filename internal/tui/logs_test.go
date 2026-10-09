package tui

import (
	"io"
	"strings"
	"testing"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
)

func TestClearLogsFormsRequireConfirmation(t *testing.T) {
	for _, name := range []string{"example", ""} {
		t.Run(name, func(t *testing.T) {
			o := testOptions(t)
			called := make(chan []string, 1)
			o.RunCommand = func(args []string, _ io.Writer) error { called <- args; return nil }
			m := newModel(o)
			if name == "" {
				m.toolsMenu()
				if !strings.Contains(m.form.View(), "Logs for all applications") {
					t.Fatal("all-app clearing unavailable in Tools")
				}
				m.allLogsMenu()
				if !strings.Contains(m.form.View(), "Clear logs") {
					t.Fatal("all-app clearing unavailable in log menu")
				}
				m.clearLogsForm(name)
			} else {
				app := config.App{Name: name}
				m.moreAppMenu(app)
				if !strings.Contains(m.form.View(), "Clear logs") {
					t.Fatal("per-app clearing unavailable")
				}
				m.appAction(app, "clear-logs")
			}
			m.next()
			selection := name
			if name == "" {
				selection = "--all"
			}
			if strings.Join(m.current.args, " ") != "logs clear "+selection+" --type all --yes" || m.approved || m.busy || m.page != "confirm" {
				t.Fatalf("clearing skipped confirmation: %v", m.current.args)
			}
			if !strings.Contains(m.reviewText, "cannot be recovered") || !strings.Contains(m.reviewText, "journals") {
				t.Fatal("review omitted scope or consequences")
			}
			press(m, tea.KeyEnter)
			select {
			case <-called:
				t.Fatal("default Cancel executed clearing")
			default:
			}
			m.clearLogsForm(name)
			press(m, tea.KeyDown)
			m.next()
			if !strings.Contains(strings.Join(m.current.args, " "), "--type application") {
				t.Fatal("application-only choice unavailable")
			}
			press(m, 'y')
			press(m, tea.KeyEnter)
			receive(t, m)
			if !strings.Contains(strings.Join(<-called, " "), "--type application --yes") || m.result != nil {
				t.Fatal("confirmed selection did not execute")
			}
			if nextStep(m.current.args) == "" {
				t.Fatal("successful clearing omitted next step")
			}
		})
	}
}
