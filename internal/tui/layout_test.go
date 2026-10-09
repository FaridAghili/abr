package tui

import (
	"fmt"
	"strings"
	"testing"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestScreensFillTerminalHeightAndKeepControlsAtBottom(t *testing.T) {
	o := testOptions(t)
	app := navigationApp("example")
	saveApps(t, o, app)
	for name, open := range map[string]func(*model){
		"main":    func(m *model) { m.home() },
		"apps":    func(m *model) { m.appsMenu() },
		"app":     func(m *model) { m.appMenu(app) },
		"tools":   func(m *model) { m.toolsMenu() },
		"input":   func(m *model) { m.cloneForm() },
		"choices": func(m *model) { m.deployForm(app.Name) },
		"details": func(m *model) { m.appDetails(app) },
		"review":  func(m *model) { m.review(action{title: "Deploy", note: strings.Repeat("Review this operation. ", 80)}) },
		"output": func(m *model) {
			m.page, m.title, m.form = "output", "Logs", nil
			m.current = action{args: []string{"logs", "example"}, note: "Recent log snapshot"}
			m.appendOutput(strings.Repeat("Log line\n", 80))
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newModel(o)
			open(m)
			for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 40}, {Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 120, Height: 40}} {
				m.Update(size)
				view := m.View()
				if !view.AltScreen || lipgloss.Height(view.Content) != size.Height || lipgloss.Width(view.Content) > size.Width {
					t.Fatalf("%s does not fill %dx%d within its bounds", name, size.Width, size.Height)
				}
				lines := strings.Split(ansi.Strip(view.Content), "\n")
				footer := strings.Split(m.footer(), "\n")
				for i, line := range footer {
					if strings.TrimSpace(lines[size.Height-1-len(footer)+i]) != line {
						t.Fatalf("%s controls moved or were clipped at %dx%d:\n%s", name, size.Width, size.Height, view.Content)
					}
				}
			}
		})
	}
}

func TestAppsListGrowsAfterResizeAndScrollsWithinBody(t *testing.T) {
	o := testOptions(t)
	var apps []config.App
	for i := range 30 {
		apps = append(apps, navigationApp(fmt.Sprintf("app-%02d", i)))
	}
	saveApps(t, o, apps...)
	m := newModel(o)
	m.appsMenu()
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	small := strings.Count(m.View().Content, "· laravel")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	large := strings.Count(m.View().Content, "· laravel")
	if large <= small {
		t.Fatal("app list did not use the added terminal height")
	}
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	before := strings.Split(ansi.Strip(m.View().Content), "\n")
	for range 29 {
		press(m, tea.KeyDown)
	}
	after := strings.Split(ansi.Strip(m.View().Content), "\n")
	if !strings.Contains(strings.Join(after, "\n"), "app-29") || !strings.Contains(strings.Join(after, "\n"), "Nightwatch: off") {
		t.Fatal("selected app's summary is not visible after scrolling")
	}
	for _, row := range []int{1, 3, 13, 14} {
		if before[row] != after[row] {
			t.Fatal("scrolling moved the header or controls")
		}
	}
}

func TestEmptyAppsListGuidesBackToMainMenu(t *testing.T) {
	m := newModel(testOptions(t))
	m.next()
	if m.page != "apps" || !strings.Contains(m.View().Content, "No apps registered") {
		t.Fatal("empty apps list lacks guidance")
	}
	m.next()
	if m.page != "home" || !strings.Contains(m.View().Content, "Register application") {
		t.Fatal("empty apps list cannot return to registration")
	}
}
