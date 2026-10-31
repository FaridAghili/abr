package tui

import (
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"abr/internal/config"
	"abr/internal/manager"
)

func TestGroupsCreateReorderDeployAndDelete(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("shop"), navigationApp("api"), navigationApp("admin"))
	// Group changes are applied as the CLI would, so menus reload them.
	groups := manager.Manager{ConfigPath: o.ConfigPath, StateDir: o.StateDir}
	var calls []string
	o.RunCommand = func(args []string, out io.Writer) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] != "group" {
			return nil
		}
		switch args[1] {
		case "create":
			return groups.CreateGroup(args[2], args[3:], true)
		case "apps":
			return groups.SetGroupApps(args[2], args[3:], true)
		case "assign":
			return groups.AssignGroups(args[2], args[3:], true)
		case "delete":
			return groups.DeleteGroup(args[2], true)
		}
		return nil
	}
	m := newModel(o)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	press(m, tea.KeyDown)
	submit(m, tea.KeyEnter) // Groups
	if m.title != "Groups" || !strings.Contains(viewText(m), "Example:") {
		t.Fatal("groups menu lacks guidance", viewText(m))
	}
	submit(m, tea.KeyEnter) // Create group
	m.Update(tea.PasteMsg{Content: "store"})
	m.form.NextGroup()
	if view := viewText(m); !strings.Contains(view, "GROUPS") || !strings.Contains(view, "Example:") {
		t.Fatal("app picker lacks groups column or guidance", view)
	}
	press(m, tea.KeyDown)
	press(m, tea.KeySpace) // api
	press(m, tea.KeyDown)
	press(m, tea.KeySpace) // shop
	m.next()
	if strings.Join(m.current.args, " ") != "group create store api shop" || !strings.Contains(m.reviewText, "Order: api → shop") {
		t.Fatal("create review inaccurate", m.current.args, m.reviewText)
	}
	approve(m)
	finishStep(t, m)
	if m.title != "Group store" || !strings.Contains(viewText(m), "Deploy order: api → shop") || !strings.Contains(m.notice, "Reorder apps") {
		t.Fatal("created group not opened with its next step", m.title, viewText(m))
	}

	// Move shop first, then save once.
	for range 3 {
		press(m, tea.KeyDown) // Reorder apps
	}
	submit(m, tea.KeyEnter)
	press(m, tea.KeyDown)
	submit(m, tea.KeyEnter) // shop
	if view := viewText(m); !strings.Contains(view, "Move shop") || !strings.Contains(view, "After api") {
		t.Fatal("move form missing", view)
	}
	press(m, tea.KeyUp)
	submit(m, tea.KeyEnter) // First
	if view := viewText(m); !strings.Contains(view, "1. shop") || !strings.Contains(view, "2. api") {
		t.Fatal("reorder not shown before saving", view)
	}
	if c, _ := config.Load(o.ConfigPath); c.Groups[0].Apps[0] != "api" {
		t.Fatal("moving saved before Save order")
	}
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	submit(m, tea.KeyEnter) // Save order
	if strings.Join(m.current.args, " ") != "group apps store shop api" {
		t.Fatal("order review inaccurate", m.current.args)
	}
	approve(m)
	finishStep(t, m)
	if !strings.Contains(viewText(m), "Deploy order: shop → api") {
		t.Fatal("saved order not shown", viewText(m))
	}

	// Redeploy the current checkout of every app in order.
	press(m, tea.KeyHome)
	submit(m, tea.KeyEnter) // Deploy group
	press(m, tea.KeyDown)
	m.next()
	if strings.Join(m.current.args, " ") != "deploy --group store --no-pull" || !strings.Contains(m.reviewText, "Order: shop → api") || !strings.Contains(m.reviewText, "the rest are skipped") || !strings.Contains(m.reviewText, "migrations") {
		t.Fatal("group deploy review inaccurate", m.current.args, m.reviewText)
	}
	press(m, tea.KeyEscape)
	press(m, tea.KeyEscape)
	if m.title != "Group store" {
		t.Fatal("did not return to the group", m.title)
	}

	// Deleting the group keeps its apps.
	for range 6 {
		press(m, tea.KeyDown) // Delete group
	}
	submit(m, tea.KeyEnter)
	if strings.Join(m.current.args, " ") != "group delete store" || !strings.Contains(m.reviewText, "are kept") {
		t.Fatal("delete review inaccurate", m.current.args, m.reviewText)
	}
	approve(m)
	finishStep(t, m)
	if c, _ := config.Load(o.ConfigPath); m.title != "Groups" || len(c.Groups) != 0 || len(c.Apps) != 3 {
		t.Fatal("group delete changed apps or stayed open", m.title, c)
	}
}

func TestAppGroupsFromTheAppMenu(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("shop"), navigationApp("api"))
	o.RunCommand = func([]string, io.Writer) error { return nil }
	m := newModel(o)
	// Without groups, the app starts a new group with itself selected.
	m.appGroupsForm(navigationApp("api"))
	if m.title != "Create group" || !strings.Contains(m.notice, "api is selected") {
		t.Fatal("no group creation offered", m.title, m.notice)
	}
	m.Update(tea.PasteMsg{Content: "backend"})
	m.form.NextGroup()
	m.next()
	if strings.Join(m.current.args, " ") != "group create backend api" {
		t.Fatal("app not preselected", m.current.args)
	}
	groups := manager.Manager{ConfigPath: o.ConfigPath, StateDir: o.StateDir}
	if err := groups.CreateGroup("backend", []string{"api"}, true); err != nil {
		t.Fatal(err)
	}
	if err := groups.CreateGroup("store", []string{"shop"}, true); err != nil {
		t.Fatal(err)
	}
	m.appsMenu()
	if view := viewText(m); !strings.Contains(view, "GROUPS") || !strings.Contains(view, "backend") {
		t.Fatal("apps table lacks groups", view)
	}
	m.appGroupsForm(navigationApp("api"))
	press(m, tea.KeyDown)
	press(m, tea.KeySpace) // join store
	m.next()
	if strings.Join(m.current.args, " ") != "group assign api backend store" || !strings.Contains(m.reviewText, "deploys last in groups it joins") {
		t.Fatal("assign review inaccurate", m.current.args, m.reviewText)
	}
}
