package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func TestAppListsAreColoredColumns(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("alpha"), navigationApp("zebra"))
	m := newModel(o)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.appsMenu()
	p := colors(m.dark)
	raw := m.View().Content
	for _, want := range []string{
		p.accent.Render("APP    "),          // Header
		p.key.Render("alpha  "),             // Name under the cursor
		p.text.Bold(true).Render("zebra  "), // Other names
		p.info.Render("zebra.example.com"),  // Domains
		p.muted.Render("off    "),           // Disabled parts
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing colored cell %q in:\n%s", want, raw)
		}
	}
	// Filtering matches the plain text, never color codes.
	typeSearch(m, "zeb")
	if view := viewText(m); strings.Contains(view, "alpha.example.com") || !strings.Contains(view, "zebra.example.com") {
		t.Fatalf("filter matched hidden color codes: %s", view)
	}
}

func TestEscReturnsToTheOpenedMenuItem(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("alpha"), navigationApp("moon"), navigationApp("zebra"))
	o.RunCommand = func([]string, io.Writer) error { return nil }
	m := newModel(o)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	hovered := func() string {
		value, _ := m.form.GetFocusedField().(*huh.Select[string]).Hovered()
		return value
	}
	for range 3 {
		press(m, tea.KeyDown) // Server & credentials
	}
	submit(m, tea.KeyEnter)
	if m.title != "Server & credentials" {
		t.Fatal("server menu not opened", m.title)
	}
	press(m, tea.KeyEscape)
	if m.page != "home" || hovered() != "server" {
		t.Fatal("main menu forgot the opened item", hovered())
	}
	press(m, tea.KeyUp)
	press(m, tea.KeyUp)
	press(m, tea.KeyUp)
	submit(m, tea.KeyEnter) // Apps
	press(m, tea.KeyDown)
	submit(m, tea.KeyEnter) // moon
	if m.title != "moon" {
		t.Fatal("app not opened", m.title)
	}
	press(m, tea.KeyDown)
	submit(m, tea.KeyEnter) // Service status runs and returns to the app menu.
	for m.busy {
		receive(t, m)
	}
	press(m, tea.KeyEscape)
	if m.title != "moon" || hovered() != "status" {
		t.Fatal("app menu forgot the opened action", m.title, hovered())
	}
	press(m, tea.KeyEscape)
	if m.page != "apps" || hovered() != "app:moon" {
		t.Fatal("apps list forgot the opened app", hovered())
	}
	press(m, tea.KeyEscape)
	if m.page != "home" || hovered() != "apps" {
		t.Fatal("main menu forgot Apps", hovered())
	}
}

// submit presses a key and applies the messages its commands produce at once,
// as the program would, so huh completes forms; timers are skipped.
func submit(m *model, code rune) {
	var apply func(tea.Cmd, int)
	apply = func(cmd tea.Cmd, depth int) {
		if cmd == nil || depth > 4 {
			return
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case msg := <-done:
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					apply(c, depth+1)
				}
			} else if msg != nil {
				_, next := m.Update(msg)
				apply(next, depth+1)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	apply(press(m, code), 0)
}
