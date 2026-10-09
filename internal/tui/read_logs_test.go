package tui

import (
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func finishLogView(t *testing.T, m *model) {
	t.Helper()
	for m.busy {
		receive(t, m)
	}
}

func TestReadLogsPerAppSourcesAndRefresh(t *testing.T) {
	for _, kind := range []string{"journal", "application", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			o := testOptions(t)
			app := navigationApp("example")
			saveApps(t, o, app)
			called := make(chan []string, 4)
			requests := 0
			o.RunCommand = func(args []string, out io.Writer) error {
				requests++
				called <- args
				_, err := fmt.Fprintf(out, "snapshot %d\n", requests)
				return err
			}
			m := newModel(o)
			m.appMenu(app)
			if !strings.Contains(m.form.View(), "Read logs") {
				t.Fatal("per-app log reader is not visible")
			}
			m.appAction(app, "logs")
			if kind == "application" || kind == "deployment" {
				press(m, tea.KeyDown)
			}
			if kind == "deployment" {
				press(m, tea.KeyDown)
			}
			m.next()
			want := "logs example --type " + kind
			if kind == "journal" {
				if m.page != "form" || !strings.Contains(m.form.View(), "Which services?") {
					t.Fatal("journal view omitted service selection")
				}
				press(m, tea.KeyDown) // Web.
				m.next()
				want = "logs example web"
			}
			finishLogView(t, m)
			if got := strings.Join(<-called, " "); got != want || !strings.Contains(m.output, "snapshot 1") || m.result != nil {
				t.Fatalf("reader executed wrong source: %s, want %s", got, want)
			}
			if !strings.Contains(m.View().Content, "r refresh") {
				t.Fatal("refresh shortcut is not shown")
			}
			press(m, 'r')
			finishLogView(t, m)
			if got := strings.Join(<-called, " "); got != want || !strings.Contains(m.output, "snapshot 2") || strings.Contains(m.output, "snapshot 1") {
				t.Fatal("refresh changed the selection or retained old output")
			}
			press(m, tea.KeyEscape)
			if m.title != app.Name || m.output != "" || m.events != nil {
				t.Fatal("leaving logs lost the app menu or retained log contents")
			}
		})
	}
}

func TestAllAppLogsShareOneScrollableView(t *testing.T) {
	for _, kind := range []string{"journal", "application", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			o := testOptions(t)
			called := make(chan []string, 2)
			o.RunCommand = func(args []string, out io.Writer) error {
				called <- args
				_, err := io.WriteString(out, "first-app log\n"+strings.Repeat("middle log\n", 30)+"second-app log\n")
				return err
			}
			m := newModel(o)
			m.toolsMenu()
			if !strings.Contains(m.form.View(), "Logs for all applications") {
				t.Fatal("all-app log menu missing from Tools")
			}
			m.allLogsMenu()
			m.next() // Read logs.
			if kind != "journal" {
				press(m, tea.KeyDown)
			}
			if kind == "deployment" {
				press(m, tea.KeyDown)
			}
			m.next()
			finishLogView(t, m)
			if got := strings.Join(<-called, " "); got != "logs --all --type "+kind || m.result != nil {
				t.Fatalf("wrong all-app log selection: %s", got)
			}
			if !strings.Contains(m.output, "first-app log") || !strings.Contains(m.output, "second-app log") {
				t.Fatal("combined view dropped an app's output")
			}
			press(m, tea.KeyHome)
			if !m.viewport.AtTop() {
				t.Fatal("combined log view cannot scroll to its first app")
			}
			press(m, tea.KeyEnd)
			if !m.viewport.AtBottom() {
				t.Fatal("combined log view cannot scroll to its last app")
			}
			press(m, tea.KeyEnter)
			if m.title != "All applications / Logs" || m.output != "" {
				t.Fatal("reader did not return to the log menu and clear output")
			}
			press(m, tea.KeyEscape)
			if m.title != "Tools" {
				t.Fatal("all-app log menu cannot return to Tools")
			}
		})
	}
}

func TestRefreshDoesNotRepeatLogClearing(t *testing.T) {
	if isLogView([]string{"logs", "clear", "--all", "--yes"}) || isLogView([]string{"deploy", "app"}) {
		t.Fatal("refresh allows destructive commands")
	}
	o := testOptions(t)
	calls := 0
	o.RunCommand = func([]string, io.Writer) error { calls++; return nil }
	m := newModel(o)
	m.start(action{title: "Clear logs", args: []string{"logs", "clear", "--all", "--yes"}})
	finishLogView(t, m)
	press(m, 'r')
	if calls != 1 || m.busy {
		t.Fatal("refresh repeated log clearing")
	}
}

func TestDiskAndLogOptionsFitSmallTerminals(t *testing.T) {
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 48, Height: 16}} {
		m := newModel(testOptions(t))
		m.Update(size)
		m.diskForm("")
		press(m, tea.KeyDown)
		view := m.View().Content
		if !strings.Contains(view, "Refresh now") || lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatalf("disk refresh option clipped at %dx%d: %s", size.Width, size.Height, view)
		}
		m.readLogsForm(nil)
		press(m, tea.KeyDown)
		press(m, tea.KeyDown)
		view = m.View().Content
		if !strings.Contains(view, "Deployment file logs") || lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatalf("log source clipped at %dx%d: %s", size.Width, size.Height, view)
		}
	}
}

func TestReadLogsForAppNamedClearDoesNotRunClearCommand(t *testing.T) {
	for _, kind := range []string{"journal", "application"} {
		o := testOptions(t)
		app := navigationApp("clear")
		saveApps(t, o, app)
		called := make(chan []string, 1)
		o.RunCommand = func(args []string, _ io.Writer) error { called <- args; return nil }
		m := newModel(o)
		m.appMenu(app)
		m.appAction(app, "logs")
		if kind == "application" {
			press(m, tea.KeyDown)
		}
		m.next()
		if kind == "journal" {
			m.next()
		}
		finishLogView(t, m)
		args := <-called
		if !isLogView(args) || args[1] == "clear" || !strings.Contains(strings.Join(args, " "), "-- clear") {
			t.Fatal("reading this app would invoke log clearing")
		}
	}
}
