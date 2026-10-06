// Package tui presents the existing CLI operations without implementing host logic.
package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"sites-manager/internal/config"
)

// Options supplies paths and a command runner shared with the noninteractive CLI.
// RunCommand must not write to the terminal directly or launch another TUI.
type Options struct {
	Version, ConfigPath, StateDir, TemplatesDir, AppsDir string
	RoadRunnerVersion                                    string
	DryRun                                               bool
	Input                                                io.Reader
	Output                                               io.Writer
	RunCommand                                           func([]string, io.Writer) error
}

func Run(o Options) error {
	if o.RunCommand == nil {
		return errors.New("TUI requires a command runner")
	}
	_, err := tea.NewProgram(newModel(o), tea.WithInput(o.Input), tea.WithOutput(o.Output)).Run()
	return err
}

type action struct {
	title string
	args  []string
	note  string
}
type event struct {
	text string
	err  error
	done bool
}
type streamWriter struct{ events chan event }

func (w streamWriter) Write(p []byte) (int, error) {
	// Bound individual messages even when a subprocess makes one enormous write.
	for start := 0; start < len(p); {
		end := min(start+4096, len(p))
		for end < len(p) && !utf8.RuneStart(p[end]) && end > start {
			end--
		}
		if end == start {
			end = min(start+4096, len(p))
		}
		w.events <- event{text: string(p[start:end])}
		start = end
	}
	return len(p), nil
}
func waitEvent(ch <-chan event) tea.Cmd { return func() tea.Msg { return <-ch } }

const outputLimit = 128 * 1024

type palette struct{ accent, muted, danger, success lipgloss.Style }

func colors(dark bool) palette {
	choose := lipgloss.LightDark(dark)
	style := func(light, dark string) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(choose(lipgloss.Color(light), lipgloss.Color(dark)))
	}
	return palette{style("#5145B5", "#8B9FFF").Bold(true), style("#59636F", "#9198A1"), style("#B4233C", "#FF838B").Bold(true), style("#187346", "#68D391").Bold(true)}
}

type model struct {
	options              Options
	dark                 bool
	width, height        int
	form                 *huh.Form
	next                 func() tea.Cmd
	page, title, context string
	current              action
	busy                 bool
	result               error
	output               string
	lineLength           int
	events               chan event
	viewport             viewport.Model
	spinner              spinner.Model
	notice               string
	approved             bool
	reviewText           string
}

func newModel(o Options) *model {
	m := &model{options: o, dark: true, width: 80, height: 24, viewport: viewport.New(viewport.WithWidth(76), viewport.WithHeight(14)), spinner: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(colors(true).accent))}
	m.viewport.SoftWrap = true
	m.home()
	return m
}
func (m *model) Init() tea.Cmd { return tea.Batch(m.form.Init(), tea.RequestBackgroundColor) }
func (m *model) setForm(page, title string, next func() tea.Cmd, groups ...*huh.Group) tea.Cmd {
	m.page, m.title, m.next = page, title, next
	m.form = huh.NewForm(groups...).WithTheme(huh.ThemeFunc(func(bool) *huh.Styles { return huh.ThemeCharm(m.dark) })).WithWidth(m.bodyWidth()).WithHeight(m.formHeight()).WithShowHelp(true)
	return m.form.Init()
}
func (m *model) bodyWidth() int  { return max(20, min(m.width-4, 96)) }
func (m *model) bodyHeight() int { return max(6, m.height-10) }
func (m *model) formHeight() int {
	if m.page == "app" {
		return max(3, m.bodyHeight()-3)
	}
	return m.bodyHeight()
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(s))
}
func (m *model) home() tea.Cmd {
	// No state mutation on opening the dashboard, and no inferred service health.
	m.output, m.result, m.notice, m.context = "", nil, "", ""
	m.lineLength = 0
	m.current = action{}
	m.reviewText = ""
	m.viewport.SetContent("")
	m.events = nil
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.notice = "No configuration yet. Set up the VPS or register your first application."
		} else {
			m.notice = clean(err.Error())
		}
	}
	choices := make([]huh.Option[string], 0, len(c.Apps)+8)
	for _, app := range c.Apps {
		choices = append(choices, huh.NewOption(clean(app.Name+"  ·  "+app.Type+"  ·  "+app.Domain), "app:"+app.Name))
	}
	choices = append(choices,
		huh.NewOption("+ Register application", "register"),
		huh.NewOption("Set up this VPS", "setup"),
		huh.NewOption("Port reservations", "ports"),
		huh.NewOption("Validate configuration", "validate"),
		huh.NewOption("Doctor / port checks", "doctor"),
		huh.NewOption("Deploy all applications", "deploy-all"),
		huh.NewOption("Reconcile port reservations", "allocate"),
		huh.NewOption("Quit", "quit"))
	var selected string
	return m.setForm("home", "Applications", func() tea.Cmd {
		if strings.HasPrefix(selected, "app:") {
			for _, app := range c.Apps {
				if app.Name == strings.TrimPrefix(selected, "app:") {
					return m.appMenu(app)
				}
			}
		}
		switch selected {
		case "quit":
			return tea.Quit
		case "register":
			return m.registerForm()
		case "setup":
			return m.setupForm()
		case "ports":
			return m.start(action{title: "Port reservations", args: []string{"ports"}})
		case "validate":
			return m.start(action{title: "Validate configuration", args: []string{"config", "validate"}})
		case "doctor":
			return m.start(action{title: "Doctor", args: []string{"doctor"}, note: "Occupied ports can belong to running managed services."})
		case "deploy-all":
			return m.deployForm("")
		case "allocate":
			return m.review(action{title: "Reconcile port reservations", args: []string{"ports", "--allocate"}, note: "Writes missing reservations. Existing assignments stay stable. Dry-run is unsupported for this command."})
		}
		return m.home()
	}, huh.NewGroup(huh.NewSelect[string]().Title(fmt.Sprintf("%d configured applications", len(c.Apps))).Description("Select an app or an operation. Press / to search.").Options(choices...).Value(&selected)))
}

func (m *model) appMenu(app config.App) tea.Cmd {
	m.notice = ""
	web := app.Web.Driver
	if app.Type == "nuxt" {
		web = "Node"
	}
	m.context = clean(fmt.Sprintf("%s · %s · %s · %s\n%s", app.Type, web, app.Domain, app.User, app.Directory))
	choices := []huh.Option[string]{
		huh.NewOption("Service status", "status"), huh.NewOption("Deploy", "deploy"),
		huh.NewOption("Enable services", "enable"), huh.NewOption("Restart services", "restart"),
		huh.NewOption("View recent logs", "logs"), huh.NewOption("Disable services", "disable"),
	}
	if app.Database.Enabled {
		choices = append(choices, huh.NewOption("Create / verify database", "database"), huh.NewOption("Reveal database credentials", "credentials"))
	}
	choices = append(choices, huh.NewOption("Remove application", "remove"), huh.NewOption("Back", "back"))
	var selected string
	return m.setForm("app", app.Name, func() tea.Cmd {
		args := []string{selected, app.Name}
		switch selected {
		case "back":
			return m.home()
		case "deploy":
			return m.deployForm(app.Name)
		case "restart", "logs":
			return m.serviceForm(app, selected)
		case "status":
			return m.start(action{title: app.Name + " / status", args: args})
		case "credentials":
			return m.review(action{title: "Reveal credentials: " + app.Name, args: []string{"database", app.Name, "--show"}, note: "Shows private database credentials on this terminal. The database is created or verified first. Output is discarded when you leave this screen."})
		case "remove":
			return m.review(action{title: "Remove " + app.Name, args: args, note: "Stops services, removes generated files and the managed Ubuntu account, and releases ports. Project files, secrets, home and database are preserved, with project/home ownership transferred to root."})
		case "disable":
			return m.review(action{title: "Disable " + app.Name, args: args, note: "Stops services and removes routing. The application will be unavailable. Users, databases and reserved ports are retained."})
		default:
			return m.review(action{title: selected + " / " + app.Name, args: args})
		}
	}, huh.NewGroup(huh.NewSelect[string]().Title("Application actions").Options(choices...).Value(&selected)))
}
func (m *model) serviceForm(app config.App, command string) tea.Cmd {
	choices := []huh.Option[string]{huh.NewOption("All services", "")}
	web := "web"
	if app.Type == "nuxt" {
		web = "nuxt"
	} else if app.Web.Driver == "octane" {
		web = "octane"
	}
	choices = append(choices, huh.NewOption("Web / "+web, web))
	if app.Queue.Workers > 0 {
		choices = append(choices, huh.NewOption("Queue workers", "queue"))
	}
	if app.Scheduler.Enabled {
		choices = append(choices, huh.NewOption("Scheduler", "scheduler"))
	}
	if app.Nightwatch.Enabled {
		choices = append(choices, huh.NewOption("Nightwatch", "nightwatch"))
	}
	if app.InertiaSSR.Enabled {
		choices = append(choices, huh.NewOption("Inertia SSR", "inertia-ssr"))
	}
	var selected string
	return m.setForm("form", command+" / "+app.Name, func() tea.Cmd {
		args := []string{command, app.Name}
		if selected != "" {
			args = append(args, selected)
		}
		a := action{title: command + " / " + app.Name, args: args}
		if command == "logs" {
			a.note = "Recent journal snapshot. For live streaming, use sites logs APP --follow outside the menu."
			return m.start(a)
		}
		return m.review(a)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Which services?").Options(choices...).Value(&selected)))
}
func (m *model) deployForm(name string) tea.Cmd {
	var noPull bool
	title := "Deploy " + name
	if name == "" {
		title = "Deploy all applications"
	}
	return m.setForm("form", title, func() tea.Cmd {
		args := []string{"deploy"}
		if name == "" {
			args = append(args, "--all")
		} else {
			args = append(args, name)
		}
		if noPull {
			args = append(args, "--no-pull")
		}
		return m.review(action{title: title, args: args, note: "Deployment has downtime: stop services, install locked dependencies, build assets, migrate Laravel, then enable services. A failure leaves the app disabled. Database and code changes are not automatically rolled back."})
	}, huh.NewGroup(huh.NewSelect[bool]().Title("Source code").Options(huh.NewOption("Pull current branch (--ff-only)", false), huh.NewOption("Use current checkout (--no-pull)", true)).Value(&noPull)))
}
func (m *model) review(a action) tea.Cmd {
	m.current = a
	m.page, m.title, m.form, m.approved, m.notice = "confirm", "Review · "+a.title, nil, false, ""
	description := a.note
	if description != "" {
		description += "\n\n"
	}
	description += "sites " + displayArgs(a.args) + "\n\nConfig: " + clean(m.options.ConfigPath) + "\nState: " + clean(m.options.StateDir) + "\nTemplates: " + clean(m.options.TemplatesDir) + "\nApps: " + clean(m.options.AppsDir)
	if m.options.DryRun {
		description += "\n\nDRY RUN: preview only. The command can reject unsupported previews."
	}
	m.reviewText = description
	m.viewport.SetContent(ansi.Wrap(description, m.bodyWidth(), ""))
	m.viewport.SetHeight(max(3, m.bodyHeight()-2))
	m.viewport.GotoTop()
	return nil
}
func displayArgs(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = strconv.Quote(clean(arg))
	}
	return strings.Join(parts, " ")
}
func (m *model) start(a action) tea.Cmd {
	a.args = append([]string(nil), a.args...)
	m.current = a
	m.page, m.title, m.form, m.output, m.result, m.busy = "output", a.title, nil, "", nil, true
	m.lineLength = 0
	m.notice, m.context = "", ""
	m.events = make(chan event, 32)
	m.viewport.SetHeight(m.bodyHeight() - 1)
	m.viewport.SetContent("")
	runner, ch, args := m.options.RunCommand, m.events, a.args
	// Only one operation is active. Completion is sent after all output, in order.
	go func() { err := runner(args, streamWriter{ch}); ch <- event{done: true, err: err} }()
	return tea.Batch(waitEvent(ch), m.spinner.Tick)
}
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.spinner.Style = colors(m.dark).accent
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.SetWidth(m.bodyWidth())
		h := m.bodyHeight() - 1
		if m.page == "confirm" {
			h--
		}
		m.viewport.SetHeight(max(3, h))
		if m.page == "confirm" {
			m.viewport.SetContent(ansi.Wrap(m.reviewText, m.bodyWidth(), ""))
		}
		if m.form != nil {
			m.form.WithWidth(m.bodyWidth()).WithHeight(m.formHeight())
		}
	case event:
		if msg.done {
			m.busy, m.result = false, msg.err
			m.notice = ""
			if msg.err != nil {
				m.appendOutput("\nError: " + clean(msg.err.Error()) + "\n")
				m.viewport.GotoBottom()
			}
			return m, nil
		}
		bottom := m.viewport.AtBottom()
		m.appendOutput(clean(msg.text))
		if bottom {
			m.viewport.GotoBottom()
		}
		return m, waitEvent(m.events)
	case tea.KeyPressMsg:
		key := msg.String()
		// Escape clears a menu search before it navigates away.
		if key == "esc" && m.form != nil {
			if field, ok := m.form.GetFocusedField().(interface{ GetFiltering() bool }); ok && field.GetFiltering() {
				updated, cmd := m.form.Update(msg)
				m.form = updated.(*huh.Form)
				return m, cmd
			}
		}
		if key == "ctrl+c" || key == "esc" {
			if m.busy {
				m.notice = "Command is still running. Wait for completion before leaving."
				return m, nil
			}
			if m.page == "home" {
				return m, tea.Quit
			}
			return m, m.home()
		}
		if m.width < 48 || m.height < 16 {
			return m, nil
		}
		if m.page == "output" && !m.busy && key == "enter" {
			return m, m.home()
		}
		if m.page == "output" || m.page == "confirm" {
			switch key {
			case "home":
				m.viewport.GotoTop()
				return m, nil
			case "end":
				m.viewport.GotoBottom()
				return m, nil
			}
		}
		if m.page == "confirm" {
			switch key {
			case "left", "right", "tab", "shift+tab":
				m.approved = !m.approved
				return m, nil
			case "y":
				m.approved = true
				return m, nil
			case "n":
				m.approved = false
				return m, nil
			case "enter":
				if m.approved {
					return m, m.start(m.current)
				}
				return m, m.home()
			}
		}
	}
	if m.page == "output" || m.page == "confirm" {
		var cmd tea.Cmd
		if m.busy {
			m.spinner, cmd = m.spinner.Update(msg)
		}
		var scroll tea.Cmd
		m.viewport, scroll = m.viewport.Update(msg)
		return m, tea.Batch(cmd, scroll)
	}
	if m.form != nil {
		updated, cmd := m.form.Update(msg)
		m.form = updated.(*huh.Form)
		if m.form.State == huh.StateCompleted {
			return m, m.next()
		}
		if m.form.State == huh.StateAborted {
			return m, m.home()
		}
		return m, cmd
	}
	return m, nil
}
func (m *model) View() tea.View {
	palette := colors(m.dark)
	accent, muted, danger, success := palette.accent, palette.muted, palette.danger, palette.success
	width := m.bodyWidth()
	mode := "LIVE · host operations require Ubuntu 26.04 AMD64 / root"
	if m.options.DryRun {
		mode = "DRY RUN · preview host operations"
	}
	header := accent.Render("SITES") + muted.Render("  "+m.options.Version+"  /  "+mode)
	header += "\n" + muted.Render(ansi.Truncate(clean(m.options.ConfigPath)+"  ·  "+clean(m.options.StateDir), width, "…"))
	var body, footer string
	if m.width < 48 || m.height < 16 {
		body = "Resize the terminal to at least 48 × 16.\nEsc / Ctrl+C goes back or quits when idle."
	} else if m.page == "output" {
		status := m.spinner.View() + " Running · leaving is disabled until completion"
		if !m.busy {
			if m.result != nil {
				status = danger.Render("Command failed · error shown below")
			} else if m.options.DryRun {
				status = success.Render("Preview finished")
			} else {
				status = success.Render("Command completed")
			}
		}
		body = lipgloss.NewStyle().Width(width).Render(status) + "\n" + m.viewport.View()
		footer = "↑/↓ scroll · pgup/pgdown page · home/end"
		if !m.busy {
			footer += " · enter/esc back"
		}
		if len(m.current.args) > 0 && (m.current.args[0] == "logs" || m.current.args[0] == "doctor") {
			footer += "\n" + ansi.Truncate(m.current.note, width, "…")
		}
	} else if m.page == "confirm" {
		cancel, run := "  Cancel  ", "  Run command  "
		if m.approved {
			run = accent.Render("[ Run command ]")
		} else {
			cancel = accent.Render("[ Cancel ]")
		}
		body = m.viewport.View() + "\n\n" + cancel + "    " + run
		footer = "←/→ choose · enter confirm · esc cancel · ↑/↓ scroll"
	} else {
		body = m.form.View()
		footer = "Esc back · Ctrl+C back / quit · menu / search"
	}
	if m.notice != "" {
		body = muted.Render(ansi.Truncate(m.notice, width, "…")) + "\n" + body
	}
	if m.context != "" && m.page == "app" {
		lines := strings.Split(m.context, "\n")
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], width, "…")
		}
		body = muted.Render(strings.Join(lines, "\n")) + "\n\n" + body
	}
	content := header + "\n\n" + accent.Render(m.title) + "\n\n" + body + "\n" + muted.Render(footer)
	// Keep every screen within the terminal, including long paths and failures.
	content = lipgloss.NewStyle().Width(width).MaxWidth(width).MaxHeight(max(1, m.height-2)).Render(content)
	v := tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(content))
	v.AltScreen = true
	return v
}

func (m *model) appendOutput(s string) {
	// Bound line length too: soft wrapping one huge subprocess line is expensive.
	var text strings.Builder
	for _, r := range s {
		if r == '\n' {
			m.lineLength = 0
		} else {
			if m.lineLength >= 256 {
				text.WriteByte('\n')
				m.lineLength = 0
			}
			m.lineLength++
		}
		text.WriteRune(r)
	}
	m.output += text.String()
	if len(m.output) > outputLimit {
		cut := len(m.output) - outputLimit
		for cut < len(m.output) && !utf8.RuneStart(m.output[cut]) {
			cut++
		}
		m.output = "[Earlier output omitted]\n" + m.output[cut:]
	}
	m.viewport.SetContent(m.output)
}
