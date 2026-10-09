// Package tui presents the existing CLI operations without implementing host logic.
package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"abr/internal/config"
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
	ComposerAuth                                         func(string, string, string, io.Writer) error
	EnvEditor                                            func(string) (*exec.Cmd, error)
	ArtisanShell                                         func(string) (*exec.Cmd, error)
}

func Run(o Options) error {
	if o.RunCommand == nil {
		return errors.New("TUI requires a command runner")
	}
	_, err := tea.NewProgram(newModel(o), tea.WithInput(o.Input), tea.WithOutput(o.Output)).Run()
	return err
}

type action struct {
	title        string
	args         []string
	note         string
	run          func(io.Writer) error // Credential operations never put tokens in args.
	after        func() tea.Cmd        // Continue immediately after successful execution.
	continueWith func() tea.Cmd        // Keep output visible until Enter continues the workflow.
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

func formTheme(dark bool) *huh.Styles {
	theme := huh.ThemeCharm(dark)
	// The stock theme uses dark text for unselected options on dark terminals.
	foreground := lipgloss.LightDark(dark)(lipgloss.Color("#25313D"), lipgloss.Color("#E2E8F0"))
	for _, field := range []*huh.FieldStyles{&theme.Focused, &theme.Blurred} {
		field.Option = field.Option.Foreground(foreground)
		field.UnselectedOption = field.UnselectedOption.Foreground(foreground)
	}
	return theme
}

type model struct {
	options              Options
	dark                 bool
	width, height        int
	form                 *huh.Form
	groups               []*huh.Group
	next                 func() tea.Cmd
	back, menu           func() tea.Cmd
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
	if page == "form" {
		m.back = m.menu
	}
	m.page, m.title, m.next = page, title, next
	m.groups = groups
	m.form = huh.NewForm(groups...).WithTheme(huh.ThemeFunc(func(bool) *huh.Styles { return formTheme(m.dark) })).WithWidth(m.bodyWidth()).WithHeight(m.formHeight()).WithShowHelp(false)
	return m.form.Init()
}
func (m *model) bodyWidth() int  { return max(20, min(m.width-4, 96)) }
func (m *model) bodyHeight() int { return max(6, m.height-8) }
func (m *model) formHeight() int {
	if m.page == "app" {
		return max(3, m.bodyHeight()-2)
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
	m.menu, m.back = m.home, nil
	// No state mutation on opening the dashboard, and no inferred service health.
	m.output, m.result, m.notice, m.context = "", nil, "", ""
	m.lineLength = 0
	m.current = action{}
	m.reviewText = ""
	m.viewport.SetContent("")
	m.events = nil
	m.next = nil
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.notice = "No configuration yet. Set up the VPS or register your first application."
		} else {
			m.notice = clean(err.Error())
		}
	}
	choices := make([]huh.Option[string], 0, len(c.Apps)+8)
	slices.SortFunc(c.Apps, func(a, b config.App) int { return strings.Compare(a.Name, b.Name) })
	for _, app := range c.Apps {
		choices = append(choices, huh.NewOption(appSummary(app), "app:"+app.Name))
	}
	choices = append(choices,
		huh.NewOption("Clone application", "clone"),
		huh.NewOption("Register application", "register"),
		huh.NewOption("Server & credentials", "server"),
		huh.NewOption("Tools", "tools"),
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
		case "clone":
			return m.cloneForm()
		case "server":
			return m.serverMenu()
		case "tools":
			return m.toolsMenu()
		}
		return m.home()
	}, huh.NewGroup(huh.NewSelect[string]().Title(fmt.Sprintf("%d configured applications", len(c.Apps))).Description("Choose an app, or clone and register a new project.").Options(choices...).Value(&selected)))
}

func (m *model) serverMenu() tea.Cmd {
	m.menu, m.back = m.serverMenu, m.home
	m.notice, m.context = "", ""
	var selected string
	return m.setForm("menu", "Server & credentials", func() tea.Cmd {
		switch selected {
		case "setup":
			return m.setupForm()
		case "update":
			return m.review(action{title: "Update server", args: []string{"update"}, note: "Upgrade apt packages, remove unused packages, clean the apt cache, self-update Composer and upgrade global npm tools, Zsh, Oh My Zsh and both shell plugins. This updates the whole VPS."})
		case "git":
			return m.gitSetupForm()
		case "composer":
			return m.composerAuthForm()
		case "mysql-admin":
			return m.start(action{title: "MySQL admin · TablePlus", args: []string{"database", "--admin", "--show"}})
		}
		return m.home()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Server actions").Description("Set up VPS guides you through GitHub, Composer and the TablePlus login.").Options(
		huh.NewOption("Set up VPS", "setup"),
		huh.NewOption("Update server", "update"),
		huh.NewOption("GitHub key", "git"),
		huh.NewOption("Composer credentials", "composer"),
		huh.NewOption("MySQL admin · TablePlus", "mysql-admin"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) toolsMenu() tea.Cmd {
	m.menu, m.back = m.toolsMenu, m.home
	m.notice, m.context = "", ""
	var selected string
	return m.setForm("menu", "Tools", func() tea.Cmd {
		switch selected {
		case "deploy":
			return m.deployForm("")
		case "backup":
			return m.databaseBackupForm("")
		case "logs":
			return m.allLogsMenu()
		case "disk":
			return m.diskForm("")
		case "ports":
			return m.start(action{title: "Port reservations", args: []string{"ports"}})
		case "validate":
			return m.start(action{title: "Validate configuration", args: []string{"config", "validate"}})
		case "doctor":
			return m.start(action{title: "Check configuration and ports", args: []string{"doctor"}, note: "Checks reserved TCP ports and expected app services."})
		case "allocate":
			return m.review(action{title: "Reconcile ports", args: []string{"ports", "--allocate"}, note: "Reserve missing ports after configuration edits. Existing assignments stay fixed. Preview is unavailable."})
		}
		return m.home()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Maintenance").Options(
		huh.NewOption("Deploy all applications", "deploy"),
		huh.NewOption("Back up databases", "backup"),
		huh.NewOption("Logs for all applications", "logs"),
		huh.NewOption("Disk usage for all applications", "disk"),
		huh.NewOption("Port reservations", "ports"),
		huh.NewOption("Validate configuration", "validate"),
		huh.NewOption("Check configuration and ports", "doctor"),
		huh.NewOption("Reconcile ports", "allocate"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) appMenu(app config.App) tea.Cmd {
	m.menu, m.back = m.appDestination(app.Name, m.appMenu), m.home
	m.notice = ""
	m.context = clean(app.Domain + " · User: " + app.User)
	choices := []huh.Option[string]{
		huh.NewOption("Deploy", "deploy"), huh.NewOption("Service status", "status"),
		huh.NewOption("Restart services", "restart"), huh.NewOption("Read logs", "logs"),
	}
	if app.Database.Enabled {
		choices = append(choices, huh.NewOption("Database", "database-menu"))
	}
	if app.Type == "laravel" {
		choices = append(choices, huh.NewOption("Artisan shell", "artisan-shell"))
	}
	choices = append(choices, huh.NewOption("More actions", "more"), huh.NewOption("Back", "back"))
	var selected string
	return m.setForm("app", app.Name, func() tea.Cmd {
		return m.appAction(app, selected)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Application actions").Options(choices...).Value(&selected)))
}

func (m *model) databaseMenu(app config.App) tea.Cmd {
	m.menu, m.back = m.appDestination(app.Name, m.databaseMenu), m.appDestination(app.Name, m.appMenu)
	var selected string
	return m.setForm("app", app.Name+" / Database", func() tea.Cmd {
		if selected == "back" {
			return m.goBack()
		}
		return m.appAction(app, selected)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Database actions").Options(
		huh.NewOption("Show credentials", "credentials"),
		huh.NewOption("Back up", "database-backup"),
		huh.NewOption("Import SQL", "database-import"),
		huh.NewOption("Create / verify", "database"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) moreAppMenu(app config.App) tea.Cmd {
	m.menu, m.back = m.appDestination(app.Name, m.moreAppMenu), m.appDestination(app.Name, m.appMenu)
	var selected string
	return m.setForm("app", app.Name+" / More actions", func() tea.Cmd {
		if selected == "back" {
			return m.goBack()
		}
		return m.appAction(app, selected)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Manage application").Options(
		huh.NewOption("Ubuntu user & paths", "user"),
		huh.NewOption("Edit settings", "edit"),
		huh.NewOption("Clear logs", "clear-logs"),
		huh.NewOption("Disk usage", "disk"),
		huh.NewOption("Enable services", "enable"),
		huh.NewOption("Disable services", "disable"),
		huh.NewOption("Remove application", "remove"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) appAction(app config.App, selected string) tea.Cmd {
	args := []string{selected, app.Name}
	switch selected {
	case "user":
		return m.appDetails(app)
	case "edit":
		return m.editMenu(app)
	case "clear-logs":
		return m.clearLogsForm(app.Name)
	case "disk":
		return m.diskForm(app.Name)
	case "back":
		return m.goBack()
	case "database-menu":
		return m.databaseMenu(app)
	case "artisan-shell":
		return m.artisanShell(app)
	case "more":
		return m.moreAppMenu(app)
	case "deploy":
		return m.deployForm(app.Name)
	case "logs":
		return m.readLogsForm(&app)
	case "restart":
		return m.serviceForm(app, selected)
	case "status":
		return m.start(action{title: app.Name + " / status", args: args})
	case "database-backup":
		return m.databaseBackupForm(app.Name)
	case "database-import":
		return m.databaseImportForm(app.Name)
	case "credentials":
		return m.review(action{title: "Show credentials: " + app.Name, args: []string{"database", app.Name, "--show"}, note: "Shows database passwords on this terminal. Copy them to the project's .env. Output clears when you leave."})
	case "remove":
		return m.removeForm(app)
	case "disable":
		return m.review(action{title: "Disable " + app.Name, args: args, note: "Stops services and routing; the app becomes unavailable. Its files, database and ports are kept."})
	case "enable":
		return m.review(action{title: "Enable " + app.Name, args: args, note: "Start this app's configured services and enable its HTTPS site."})
	case "database":
		return m.review(action{title: "Verify database: " + app.Name, args: args, note: "Create or verify the managed database. Existing data and passwords are kept."})
	}
	return m.appMenu(app)
}

func (m *model) removeForm(app config.App) tea.Cmd {
	purge := true
	return m.setForm("form", "Remove "+app.Name, func() tea.Cmd {
		a := action{title: "Remove " + app.Name, args: []string{"remove", app.Name}, note: "Stops this app and removes its managed services, user, configuration and port reservations. Project files, uploads, home, database and credentials are kept."}
		if purge {
			a.title = "Fully delete " + app.Name
			a.args = append(a.args, "--purge", "--yes")
			a.note = "Permanently delete " + app.Directory + " (including .env and uploads), the app's home, managed database and DB user, credentials, deployment history, services, configuration and port reservations. This cannot be undone. Shared server tools, shared Composer/Git credentials, self-managed databases and separately exported backups stay."
		}
		return m.review(a)
	}, huh.NewGroup(huh.NewSelect[bool]().Title("What should be removed?").Description("Keep data for later, or permanently delete this app and its data.\nExample: full deletion for an app you no longer need.").Options(
		huh.NewOption("Fully delete app and data", true),
		huh.NewOption("Remove services; keep files and database", false)).Value(&purge)))
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
		if command == "logs" && app.Name == "clear" {
			args = []string{"logs", "--", app.Name}
		}
		if selected != "" {
			args = append(args, selected)
		}
		a := action{title: command + " / " + app.Name, args: args}
		if command == "logs" {
			a.note = "Recent journal snapshot · r refresh · CLI --follow streams journals."
			return m.start(a)
		}
		return m.review(a)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Which services?").Description("Choose one service, or all services for this app.\nExample: the web service serving visitors.").Options(choices...).Value(&selected)))
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
		note := "The app has downtime while dependencies are installed, assets are built and services restart. Failures are reported; completed changes are not automatically rolled back."
		if c, err := config.Load(m.options.ConfigPath); err == nil {
			for _, app := range c.Apps {
				if app.Type == "laravel" && (name == "" || app.Name == name) {
					note += " Laravel deployments also run database migrations."
					break
				}
			}
		}
		return m.review(action{title: title, args: args, note: note})
	}, huh.NewGroup(huh.NewSelect[bool]().Title("Source code").Description("Pull your latest committed code, or deploy the current files.\nExample: current checkout for a first deployment after cloning.").Options(huh.NewOption("Pull latest code", false), huh.NewOption("Use current checkout", true)).Value(&noPull)))
}
func (m *model) review(a action) tea.Cmd {
	if m.form != nil && m.page == "form" {
		groups, focused, next, back := m.groups, m.form.GetFocusedField(), m.next, m.back
		page, title, context := m.page, m.title, m.context
		m.back = func() tea.Cmd {
			m.context = context
			cmds := []tea.Cmd{m.setForm(page, title, next, groups...)}
			m.back = back
			// A completed huh form no longer renders. Rebuild it with the same
			// fields and return to the field the user just submitted.
			for i := 0; i < len(groups) && m.form.GetFocusedField() != focused; i++ {
				cmds = append(cmds, m.form.NextGroup())
			}
			return tea.Sequence(cmds...)
		}
	} else {
		m.back = m.menu
	}
	m.current = a
	m.page, m.title, m.form, m.approved, m.notice = "confirm", "Review · "+a.title, nil, false, ""
	description := a.note
	if description == "" {
		description = "Run this action?"
	}
	if m.options.DryRun {
		description += "\n\nPreview only; no host changes."
	}
	m.reviewText = clean(description)
	m.viewport.SetContent(ansi.Wrap(m.reviewText, m.bodyWidth(), ""))
	m.viewport.SetHeight(max(3, m.bodyHeight()-2))
	m.viewport.GotoTop()
	return nil
}
func (m *model) start(a action) tea.Cmd {
	// Return to the originating menu after execution, never to a retained
	// credential form or a confirmation that could repeat the command.
	m.back = m.menu
	a.args = append([]string(nil), a.args...)
	m.current = a
	m.current.run = nil
	m.page, m.title, m.form, m.output, m.result, m.busy = "output", a.title, nil, "", nil, true
	m.groups = nil
	m.next = nil
	m.lineLength = 0
	m.notice, m.context = "", ""
	m.events = make(chan event, 32)
	m.viewport.SetHeight(m.bodyHeight() - 1)
	m.viewport.SetContent("")
	runner, ch, args := m.options.RunCommand, m.events, a.args
	// Only one operation is active. Completion is sent after all output, in order.
	go func() {
		var err error
		if a.run != nil {
			err = a.run(streamWriter{ch})
		} else {
			err = runner(args, streamWriter{ch})
		}
		ch <- event{done: true, err: err}
	}()
	return tea.Batch(waitEvent(ch), m.spinner.Tick)
}
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case artisanShellFinished:
		m.busy = false
		if msg.err != nil {
			return m, m.workflowError("Artisan shell: "+msg.name, msg.err)
		}
		return m, m.appDestination(msg.name, m.appMenu)()
	case editorFinished:
		if msg.err != nil {
			return m, m.workflowError("Edit .env: "+msg.name, msg.err)
		}
		return m, m.firstDeploy(msg.name)
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.spinner.Style = colors(m.dark).accent
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.SetWidth(m.bodyWidth())
		h := m.bodyHeight() - 1
		if m.page == "confirm" {
			h--
		} else if m.page == "details" {
			h = m.bodyHeight()
		}
		m.viewport.SetHeight(max(3, h))
		if m.page == "confirm" || m.page == "details" {
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
				m.current.after, m.current.continueWith = nil, nil
				m.appendOutput("\nError: " + clean(msg.err.Error()) + "\n")
				m.viewport.GotoBottom()
			} else if !m.options.DryRun {
				if after := m.current.after; after != nil {
					m.current.after = nil
					return m, after()
				}
				if m.current.continueWith != nil {
					m.viewport.GotoBottom()
					return m, nil
				}
				if hint := nextStep(m.current.args); hint != "" {
					m.appendOutput("\nNext: " + hint + "\n")
					m.viewport.GotoBottom()
				}
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
			return m, m.goBack()
		}
		if m.width < 48 || m.height < 16 {
			return m, nil
		}
		if m.page == "details" && key == "enter" {
			return m, m.goBack()
		}
		if m.page == "output" && !m.busy && key == "enter" {
			if next := m.current.continueWith; next != nil && m.result == nil && !m.options.DryRun {
				m.current.continueWith = nil
				return m, next()
			}
			return m, m.goBack()
		}
		if m.page == "output" && !m.busy && key == "r" && isLogView(m.current.args) {
			return m, m.start(m.current)
		}
		if m.page == "output" || m.page == "confirm" || m.page == "details" {
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
				return m, m.goBack()
			}
		}
	}
	if m.page == "output" || m.page == "confirm" || m.page == "details" {
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
			return m, m.goBack()
		}
		return m, cmd
	}
	return m, nil
}
func (m *model) View() tea.View {
	palette := colors(m.dark)
	accent, muted, danger, success := palette.accent, palette.muted, palette.danger, palette.success
	width := m.bodyWidth()
	mode := ""
	if m.options.DryRun {
		mode = " · Preview"
	}
	header := accent.Render("Abr") + muted.Render("  "+m.options.Version+mode)
	var body, footer string
	if m.width < 48 || m.height < 16 {
		body = "Resize the terminal to at least 48 × 16.\nEsc / Ctrl+C goes back or quits when idle."
	} else if m.page == "details" {
		body = m.viewport.View()
		footer = "↑/↓ scroll · Enter / Esc back"
	} else if m.page == "output" {
		status := m.spinner.View() + " Running"
		if !m.busy {
			if m.result != nil {
				status = danger.Render("Command failed · error shown below")
				if len(m.current.args) > 0 && m.current.args[0] == "doctor" {
					status = danger.Render("Checks need attention · details below")
				}
			} else if m.options.DryRun {
				status = success.Render("Preview finished")
			} else {
				status = success.Render("Command completed")
				if len(m.current.args) > 0 && m.current.args[0] == "doctor" {
					status = success.Render("Checks passed")
				}
			}
		}
		body = lipgloss.NewStyle().Width(width).Render(status) + "\n" + m.viewport.View()
		footer = "↑/↓ scroll"
		if !m.busy && isLogView(m.current.args) {
			footer += " · r refresh"
		}
		if !m.busy {
			if m.current.continueWith != nil && m.result == nil && !m.options.DryRun {
				footer += " · Enter continue · Esc cancel"
			} else {
				footer += " · Enter / Esc back"
			}
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
		footer = "←/→ choose · enter confirm · esc cancel"
	} else {
		body = m.form.View()
		footer = "↑/↓ select · Enter open · Esc back"
		if m.page == "form" {
			footer = "Enter next · Shift+Tab back · Esc cancel"
			switch m.form.GetFocusedField().(type) {
			case *huh.MultiSelect[string]:
				footer = "Space toggle · Enter next · Esc cancel"
			case *huh.Select[string], *huh.Select[bool]:
				footer = "↑/↓ choose · Enter next · Esc cancel"
			}
		}
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

func nextStep(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "edit":
		return "Choose Deploy to apply saved settings. Database credentials and data are retained when a component is disabled."
	case "disk":
		return "Choose Disk usage again and Refresh now for a new app scan. Clear logs from More actions or Tools when needed."
	case "logs":
		if len(args) > 1 && args[1] == "clear" {
			return "Choose Read logs to inspect app activity. New file logs continue to be written by the app."
		}
	case "setup":
		return "Open Server & credentials to set up the GitHub key, then clone your project. MySQL admin shows your TablePlus login."
	case "update":
		return "Open an app’s Service status to check its services, then deploy when ready."
	case "git":
		return "Add the public key to GitHub Settings → SSH and GPG keys, then choose Clone application."
	case "clone":
		return "Choose Register application and use the same app name. Its project path is automatic."
	case "register":
		if slices.Contains(args, "--config-only") {
			return "Register on the VPS before deploying; this saved local configuration only."
		}
		kind := slices.Index(args, "--type")
		if (kind >= 0 && kind+1 < len(args) && args[kind+1] == "nuxt") || slices.Contains(args, "--no-database") {
			return "Prepare the project's .env, then choose Deploy from its app menu."
		}
		return "Prepare the project's .env. Copy database values from the app's Database menu, then choose Deploy."
	case "composer":
		return "Choose Deploy for your app. Saved credentials are reused automatically."
	case "database":
		if slices.Contains(args, "--admin") {
			return "In TablePlus choose MySQL with SSH, use the database values above and your existing VPS SSH login."
		}
		if len(args) > 1 && args[1] == "backup" {
			return "Copy the SQL backups to storage outside this VPS."
		}
		if len(args) > 1 && args[1] == "import" {
			return "Check the restored data, then enable the app's services."
		}
		return "Copy the database values into the project's .env before deploying."
	}
	return ""
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
