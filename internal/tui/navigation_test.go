package tui

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

func saveApps(t *testing.T, o Options, apps ...config.App) {
	t.Helper()
	c := config.Default()
	c.Apps = apps
	data, err := config.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func navigationApp(name string) config.App {
	return config.App{Name: name, Type: "laravel", Domain: name + ".example.com", Directory: "/srv/apps/" + name, User: "custom-" + name, Web: config.Web{Driver: "fpm"}, Database: config.Database{Enabled: true}}
}

func TestDashboardSortsAppsAndShowsConfiguredSettings(t *testing.T) {
	o := testOptions(t)
	z, a := navigationApp("zebra"), navigationApp("alpha")
	z.Web = config.Web{Driver: "octane", Workers: 4}
	z.Queue.Workers, z.Scheduler.Enabled, z.Nightwatch.Enabled = 3, true, true
	saveApps(t, o, z, a)
	m := newModel(o)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := m.View().Content
	if strings.Index(view, "alpha · laravel") < 0 || strings.Index(view, "alpha · laravel") >= strings.Index(view, "zebra · laravel") {
		t.Fatalf("projects not sorted: %s", view)
	}
	for _, settings := range []string{"FPM · Queue: 0 · Scheduler: off · Nightwatch: off", "Octane (4) · Queue: 3 · Scheduler: on · Nightwatch: on"} {
		if !strings.Contains(view, settings) {
			t.Fatalf("missing %q: %s", settings, view)
		}
	}
	// The first selection must match the displayed order, not config file order.
	m.next()
	if m.title != "alpha" {
		t.Fatalf("sorted entry selected wrong app: %s", m.title)
	}
	c, err := config.Load(o.ConfigPath)
	if err != nil || c.Apps[0].Name != "zebra" {
		t.Fatal("viewing the list modified configuration")
	}

	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 48, Height: 16}} {
		m.home()
		m.Update(size)
		view = m.View().Content
		for _, label := range []string{"FPM", "Queue: 0", "Scheduler: off", "Nightwatch: off", "Esc back"} {
			if !strings.Contains(view, label) {
				t.Fatalf("summary clipped at %dx%d: %s", size.Width, size.Height, view)
			}
		}
		if lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatal("dashboard exceeds terminal bounds")
		}
	}
}

func TestEscReturnsOneLevelThroughProjectMenus(t *testing.T) {
	o := testOptions(t)
	app := navigationApp("example")
	saveApps(t, o, app)
	m := newModel(o)
	m.next() // Home → app.
	m.appAction(app, "more")
	m.appAction(app, "edit")
	m.editForm(app, "workers")
	for _, title := range []string{"example / Edit settings", "example / More actions", "example", "Applications"} {
		press(m, tea.KeyEscape)
		if m.title != title {
			t.Fatalf("Esc returned to %q, want %q", m.title, title)
		}
	}
	m.next()
	m.appAction(app, "database-menu")
	m.appAction(app, "database-import")
	press(m, tea.KeyEscape)
	if m.title != "example / Database" {
		t.Fatal("import did not return to database menu")
	}
	press(m, tea.KeyEscape)
	if m.title != "example" {
		t.Fatal("database menu did not return to project")
	}
}

func TestMenuBackChoicesAndSearchEscape(t *testing.T) {
	o := testOptions(t)
	app := navigationApp("example")
	saveApps(t, o, app)
	m := newModel(o)
	m.appMenu(app)
	m.moreAppMenu(app)
	m.editMenu(app)
	for _, title := range []string{"example / More actions", "example", "Applications"} {
		// All application menus put Back at the end.
		press(m, tea.KeyUp)
		m.next()
		if m.title != title {
			t.Fatalf("Back returned to %q, want %q", m.title, title)
		}
	}
	m.appMenu(app)
	m.moreAppMenu(app)
	press(m, '/')
	press(m, tea.KeyEscape)
	if m.title != "example / More actions" {
		t.Fatal("Esc navigated away instead of closing menu search")
	}
	press(m, tea.KeyEscape)
	if m.title != "example" {
		t.Fatal("Esc did not navigate after search closed")
	}
}

func TestReviewCancelPreservesFormAndOutputReturnsToFreshMenu(t *testing.T) {
	o := testOptions(t)
	app := navigationApp("example")
	saveApps(t, o, app)
	o.RunCommand = func(_ []string, out io.Writer) error {
		app.Queue.Workers = 7
		saveApps(t, o, app)
		_, err := io.WriteString(out, "saved\n")
		return err
	}
	m := newModel(o)
	m.appMenu(app)
	m.moreAppMenu(app)
	m.editMenu(app)
	m.editForm(app, "workers")
	m.form.NextGroup() // Queue workers.
	m.Update(tea.PasteMsg{Content: "5"})
	m.form.NextGroup()                                 // Complete form.
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}) // Process completion → review.
	if m.page != "confirm" {
		t.Fatal("form did not reach review")
	}
	press(m, tea.KeyEscape)
	if m.page != "form" || m.form.State != huh.StateNormal || !strings.Contains(m.View().Content, "Queue workers") || !strings.Contains(m.View().Content, "05") {
		t.Fatalf("review cancel lost form: %s", m.View().Content)
	}
	m.form.NextGroup()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.page != "confirm" {
		t.Fatal("returned form cannot be submitted again")
	}
	approve(m)
	finishStep(t, m)
	press(m, tea.KeyEnter)
	if m.title != "example / Edit settings" || m.output != "" || m.current.run != nil {
		t.Fatal("output did not return to originating menu or clear operation")
	}
	// Select workers on the newly loaded menu and inspect the saved value.
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	m.next()
	m.next()
	if !strings.Contains(strings.Join(m.current.args, " "), "--queue-workers 7") {
		t.Fatal("returning to the menu retained stale app settings")
	}
}

func TestUserDetailsShowConfiguredAccountAndUploadPath(t *testing.T) {
	o := testOptions(t)
	app := navigationApp("example")
	saveApps(t, o, app)
	o.RunCommand = func([]string, io.Writer) error { t.Error("viewing user details executed a command"); return nil }
	m := newModel(o)
	m.appMenu(app)
	m.moreAppMenu(app)
	m.next() // Ubuntu user & paths.
	if m.page != "details" {
		t.Fatal("user details option is inaccessible")
	}
	for _, text := range []string{"Configured Ubuntu user: custom-example", "Managed group: custom-example", app.Directory, filepath.Join(app.Directory, "storage/app/public")} {
		if !strings.Contains(m.reviewText, text) {
			t.Fatalf("missing %q in details: %s", text, m.reviewText)
		}
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}} {
		m.Update(size)
		view := m.View().Content
		if lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height || !strings.Contains(view, "Enter / Esc back") {
			t.Fatalf("details exceed terminal bounds: %s", view)
		}
	}
	press(m, tea.KeyEscape)
	if m.title != "example / More actions" || m.viewport.GetContent() != "" {
		t.Fatal("details did not return to parent menu and clear")
	}
	app.Type = "nuxt"
	m.appDetails(app)
	if strings.Contains(m.reviewText, "storage/app/public") {
		t.Fatal("Nuxt details show a Laravel upload path")
	}
}
