package tui

import (
	"errors"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[ports]\nfirst = 10000\nlast = 19999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{Version: "0.1.0", ConfigPath: path, StateDir: filepath.Join(dir, "state"), TemplatesDir: filepath.Join(dir, "templates"), AppsDir: filepath.Join(dir, "apps"), RoadRunnerVersion: "2025.1.15"}
}
func press(m *model, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}
func receive(t *testing.T, m *model) event {
	t.Helper()
	select {
	case msg := <-m.events:
		m.Update(msg)
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not finish")
		return event{}
	}
}
func TestReviewRequiresExplicitApproval(t *testing.T) {
	o := testOptions(t)
	called := make(chan []string, 1)
	o.RunCommand = func(args []string, w io.Writer) error { called <- args; return nil }
	m := newModel(o)
	a := action{title: "Remove example", args: []string{"remove", "example"}, note: "Preserve files and database."}
	m.review(a)
	if m.approved {
		t.Fatal("default approval must be Cancel")
	}
	press(m, tea.KeyEnter)
	if m.page != "home" {
		t.Fatal("cancel did not return home")
	}
	select {
	case <-called:
		t.Fatal("cancel executed a command")
	default:
	}
	m.review(a)
	press(m, 'y')
	if m.busy {
		t.Fatal("selection executed without Enter")
	}
	press(m, tea.KeyEnter)
	receive(t, m)
	args := <-called
	if strings.Join(args, " ") != "remove example" {
		t.Fatalf("wrong command: %v", args)
	}
	if m.busy || m.result != nil {
		t.Fatal("operation not completed")
	}
}
func TestRunningOperationCannotBeAbandonedOrDuplicated(t *testing.T) {
	o := testOptions(t)
	release := make(chan struct{})
	o.RunCommand = func(args []string, w io.Writer) error { <-release; return errors.New("build failed") }
	m := newModel(o)
	m.start(action{title: "Deploy", args: []string{"deploy", "example"}})
	for _, k := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}, {Code: tea.KeyEnter}} {
		m.Update(k)
	}
	if !m.busy || m.page != "output" {
		t.Fatal("running operation was abandoned")
	}
	close(release)
	receive(t, m)
	if m.result == nil || !strings.Contains(m.View().Content, "Command failed") || strings.Contains(m.View().Content, "Command completed") {
		t.Fatal("failure was reported as success")
	}
	if !strings.Contains(m.output, "build failed") {
		t.Fatal("missing backend error")
	}
	press(m, tea.KeyEnter)
	if m.page != "home" {
		t.Fatal("completed operation did not return home")
	}
}
func TestOutputIsBoundedSanitizedAndCleared(t *testing.T) {
	o := testOptions(t)
	o.RunCommand = func(args []string, w io.Writer) error {
		_, err := io.WriteString(w, strings.Repeat("界", outputLimit)+"\x1b[2J\x1b]52;c;c2VjcmV0\a\nDB_PASSWORD=secret\n")
		return err
	}
	m := newModel(o)
	m.start(action{title: "Credentials", args: []string{"database", "app", "--show"}})
	for !receive(t, m).done {
	}
	if len(m.output) > outputLimit+64 || !utf8.ValidString(m.output) {
		t.Fatal("output not bounded / invalid UTF-8")
	}
	if strings.ContainsAny(m.output, "\x1b\a") {
		t.Fatal("terminal controls escaped sanitization")
	}
	if !strings.Contains(m.output, "DB_PASSWORD=secret") {
		t.Fatal("explicit credentials missing")
	}
	press(m, tea.KeyEscape)
	if m.output != "" || m.viewport.GetContent() != "" || strings.Contains(m.View().Content, "secret") {
		t.Fatal("credentials retained after leaving")
	}
}
func TestTerminalSizingAndScrollableReview(t *testing.T) {
	m := newModel(testOptions(t))
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 48, Height: 16}, {Width: 120, Height: 40}} {
		m.Update(size)
		m.review(action{title: "Deploy", args: []string{"deploy", "app"}, note: strings.Repeat("A long operation description. ", 100)})
		view := m.View().Content
		if lipgloss.Height(view) > size.Height || lipgloss.Width(view) > size.Width {
			t.Fatalf("view exceeds terminal %dx%d", size.Width, size.Height)
		}
		if !strings.Contains(view, "[ Cancel ]") || !strings.Contains(view, "enter confirm") {
			t.Fatalf("confirmation controls not visible at %dx%d", size.Width, size.Height)
		}
		before := m.viewport.YOffset()
		press(m, tea.KeyDown)
		if m.viewport.YOffset() <= before {
			t.Fatalf("review cannot scroll at %dx%d: offset %d, lines %d, height %d", size.Width, size.Height, m.viewport.YOffset(), m.viewport.TotalLineCount(), m.viewport.Height())
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	if !strings.Contains(m.View().Content, "Resize") {
		t.Fatal("no small-terminal guidance")
	}
	press(m, 'y')
	if m.approved {
		t.Fatal("hidden confirmation accepted input")
	}
}
func TestOpeningDoesNotCreateState(t *testing.T) {
	o := testOptions(t)
	newModel(o)
	if _, err := os.Stat(o.StateDir); !os.IsNotExist(err) {
		t.Fatalf("opening dashboard wrote state: %v", err)
	}
	if err := os.WriteFile(o.ConfigPath, []byte("unknown = true"), 0600); err != nil {
		t.Fatal(err)
	}
	m := newModel(o)
	if !strings.Contains(m.notice, "decode config") {
		t.Fatal("corrupt configuration hidden")
	}
}

func TestGuidedDefaults(t *testing.T) {
	m := newModel(testOptions(t))
	m.setupForm()
	m.next()
	for _, arg := range m.current.args {
		if strings.HasPrefix(arg, "--no-") {
			t.Fatalf("setup default disabled a requested component: %v", m.current.args)
		}
	}
	if m.page != "confirm" || m.approved {
		t.Fatal("setup skipped review")
	}
	m.registerForm()
	m.next()
	args := strings.Join(m.current.args, " ")
	if !strings.Contains(args, "--web-driver fpm") || !strings.Contains(args, "--queue-workers 0") || !strings.Contains(args, "--canonical-host as-entered") || strings.Contains(args, "--no-database") || strings.Contains(args, "--octane-workers") {
		t.Fatalf("unexpected Laravel defaults: %s", args)
	}
	if m.page != "confirm" || m.approved {
		t.Fatal("registration skipped review")
	}
}

func TestAppMenuFitsSmallTerminal(t *testing.T) {
	m := newModel(testOptions(t))
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	m.appMenu(config.App{Name: "example", Directory: "/srv/apps/example", User: "abr-example", Type: "laravel", Domain: "example.com", Web: config.Web{Driver: "octane"}, Database: config.Database{Enabled: true}})
	if view := m.View().Content; !strings.Contains(view, "Esc back") {
		t.Fatalf("app navigation clipped: %q", view)
	}
}

func TestTerminalThemePersistsAcrossForms(t *testing.T) {
	m := newModel(testOptions(t))
	for _, dark := range []bool{false, true} {
		background := color.RGBA{255, 255, 255, 255}
		if dark {
			background = color.RGBA{0, 0, 0, 255}
		}
		m.Update(tea.BackgroundColorMsg{Color: background})
		m.setupForm()
		if m.dark != dark {
			t.Fatal("terminal theme lost when opening a new form")
		}
		if got, want := m.spinner.Style.GetForeground(), colors(dark).accent.GetForeground(); got != want {
			t.Fatal("spinner ignored terminal theme")
		}
	}
}
