package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func typeSearch(m *model, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestTypingFiltersMenusAndOpensHighlightedApp(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("alpha"), navigationApp("moon"), navigationApp("zebra"))
	m := newModel(o)
	typeSearch(m, "APPs")
	field := m.form.GetFocusedField().(*huh.Select[string])
	if value, ok := field.Hovered(); !ok || value != "apps" || !field.GetFiltering() {
		t.Fatal("typing did not filter the main menu")
	}
	press(m, tea.KeyEnter)
	m.next()
	if m.page != "apps" {
		t.Fatal("filtered main menu did not open Apps")
	}
	typeSearch(m, "MOON")
	view := m.View().Content
	if !strings.Contains(view, "moon · laravel") || strings.Contains(view, "alpha · laravel") || strings.Contains(view, "zebra · laravel") {
		t.Fatalf("apps search did not filter case-insensitively: %s", view)
	}
	press(m, tea.KeyEnter)
	m.next()
	if m.title != "moon" {
		t.Fatal("filtered selection opened the wrong app")
	}
	typeSearch(m, "more")
	field = m.form.GetFocusedField().(*huh.Select[string])
	if value, ok := field.Hovered(); !ok || value != "more" {
		t.Fatal("application actions cannot be searched")
	}
	press(m, tea.KeyEscape)
	if m.title != "moon" || m.listField().GetFiltering() || !strings.Contains(m.View().Content, "Deploy") {
		t.Fatal("Esc did not clear the search on the current menu")
	}
	press(m, tea.KeyEscape)
	if m.page != "apps" {
		t.Fatal("Esc did not return from app to Apps")
	}
}

func TestListSearchLettersAreNotNavigationShortcuts(t *testing.T) {
	for _, query := range []string{"j", "k", "g", "G", "x", "界"} {
		t.Run(query, func(t *testing.T) {
			m := newModel(testOptions(t))
			var selected string
			m.setForm("menu", "Search", nil, huh.NewGroup(huh.NewSelect[string]().Title("Choose").Options(
				huh.NewOption("Other", "other"), huh.NewOption(query+" match", "match")).Value(&selected)))
			typeSearch(m, query)
			field := m.form.GetFocusedField().(*huh.Select[string])
			if value, ok := field.Hovered(); !ok || value != "match" || !field.GetFiltering() || !strings.Contains(ansi.Strip(m.View().Content), "/"+query) {
				t.Fatal("letter key navigated instead of filtering")
			}
		})
	}
}

func TestSearchBackspaceNoMatchesAndEscape(t *testing.T) {
	m := newModel(testOptions(t))
	m.serverMenu()
	typeSearch(m, "mysqlz")
	for _, code := range []rune{tea.KeyUp, tea.KeyDown, tea.KeyHome, tea.KeyEnd, tea.KeyEnter} {
		press(m, code)
	}
	if !m.noSearchMatch() || m.title != "Server & credentials" || m.busy {
		t.Fatal("empty search moved or executed a stale selection")
	}
	press(m, tea.KeyBackspace)
	field := m.form.GetFocusedField().(*huh.Select[string])
	if value, ok := field.Hovered(); !ok || value != "mysql-admin" {
		t.Fatal("Backspace did not recover the search result")
	}
	press(m, tea.KeyEscape)
	if field.GetFiltering() || m.title != "Server & credentials" || !strings.Contains(m.View().Content, "Set up VPS") {
		t.Fatal("Esc did not restore the full menu")
	}
	press(m, tea.KeyEscape)
	if m.page != "home" {
		t.Fatal("second Esc did not go back")
	}
}

func TestSearchWorksInChoicesAndMultiSelectWithoutChangingTextInputs(t *testing.T) {
	m := newModel(testOptions(t))
	m.deployForm("example")
	typeSearch(m, "current")
	press(m, tea.KeyEnter)
	m.next()
	if !strings.Contains(strings.Join(m.current.args, " "), "--no-pull") || m.approved {
		t.Fatal("boolean search lost the choice or skipped review")
	}
	var selected []string
	m.setForm("form", "Components", nil, huh.NewGroup(huh.NewMultiSelect[string]().Title("Choose").Options(
		huh.NewOption("Queue", "queue"), huh.NewOption("Scheduler", "scheduler"), huh.NewOption("Nightwatch", "nightwatch")).Value(&selected)))
	typeSearch(m, "sched")
	press(m, tea.KeySpace)
	if strings.Join(selected, " ") != "scheduler" {
		t.Fatal("Space did not toggle a filtered multi-selection")
	}
	// A retained filter must also clear before leaving the form.
	press(m, tea.KeyEscape)
	if m.page != "form" || !strings.Contains(m.View().Content, "Nightwatch") {
		t.Fatal("Esc left the form with a retained filter")
	}
	typeSearch(m, "zzzz")
	press(m, tea.KeyUp)
	press(m, tea.KeySpace)
	press(m, tea.KeyEnter)
	if strings.Join(selected, " ") != "scheduler" || !m.noSearchMatch() {
		t.Fatal("empty multi-selection toggled a hidden choice")
	}
	press(m, tea.KeyEscape)
	m.cloneForm()
	typeSearch(m, "git@github.com:owner/project.git")
	if m.listField() != nil || !strings.Contains(m.View().Content, "git@github.com:owner/project.git") {
		t.Fatal("list search intercepted normal input")
	}
}

func TestSearchNavigationAndFilteringScrollLongLists(t *testing.T) {
	m := newModel(testOptions(t))
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	var selected string
	var options []huh.Option[string]
	for i := range 50 {
		options = append(options, huh.NewOption(fmt.Sprintf("Match %02d", i), fmt.Sprintf("%02d", i)))
	}
	m.setForm("menu", "Matches", nil, huh.NewGroup(huh.NewSelect[string]().Title("Choose").Options(options...).Value(&selected)))
	typeSearch(m, "match")
	for range 49 {
		press(m, tea.KeyDown)
	}
	if value, ok := m.form.GetFocusedField().(*huh.Select[string]).Hovered(); !ok || value != "49" {
		t.Fatal("cannot navigate filtered results")
	}
	if view := m.View().Content; !strings.Contains(view, "Match 49") || !strings.Contains(view, "Esc back") || !strings.Contains(view, "Esc clears search") {
		t.Fatalf("selected match or controls scrolled out of view: %s", view)
	}
}
