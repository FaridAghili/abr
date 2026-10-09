package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func (m *model) editMenu(app config.App) tea.Cmd {
	m.menu, m.back = m.appDestination(app.Name, m.editMenu), m.appDestination(app.Name, m.moreAppMenu)
	var selected string
	choices := []huh.Option[string]{huh.NewOption("Domains", "domains"), huh.NewOption("Health check", "health")}
	if app.Type == "laravel" {
		choices = append(choices, huh.NewOption("Workers", "workers"), huh.NewOption("Components", "components"), huh.NewOption("Build order", "build-order"))
	}
	choices = append(choices, huh.NewOption("Iframe embedding", "embedding"), huh.NewOption("Back", "back"))
	return m.setForm("app", app.Name+" / Edit settings", func() tea.Cmd {
		if selected == "back" {
			return m.goBack()
		}
		return m.editForm(app, selected)
	}, huh.NewGroup(huh.NewSelect[string]().Title("What would you like to change?").Options(choices...).Value(&selected)))
}

func (m *model) editForm(app config.App, section string) tea.Cmd {
	m.context, m.notice = "", ""
	var groups []*huh.Group
	var flags func() []string
	switch section {
	case "domains":
		domain, aliases, domains := app.Domain, strings.Join(app.Aliases, ", "), strings.Join(app.Domains, ", ")
		canonical := config.CanonicalAsEntered
		groups = []*huh.Group{
			huh.NewGroup(textInput("Primary domain", "Hostname serving this app. Point its DNS to this VPS.", "example.com", &domain).Validate(required)),
			huh.NewGroup(huh.NewSelect[string]().Title("Canonical host").Description("Choose the preferred address. As entered keeps the current aliases.\nExample: prefer non-www to redirect www.example.com.").Options(huh.NewOption("As entered", config.CanonicalAsEntered), huh.NewOption("Prefer www", config.CanonicalWWW), huh.NewOption("Prefer non-www", config.CanonicalNonWWW)).Value(&canonical)),
			huh.NewGroup(textInput("Redirect domains", "Comma-separated domains that redirect. Clear to remove all.", "old.example.com", &aliases)),
			huh.NewGroup(textInput("Extra serving domains", "Comma-separated domains serving this app. Clear to remove all.", "shop.example.com", &domains)),
		}
		flags = func() []string {
			args := []string{"--domain", domain, "--canonical-host", canonical}
			for _, item := range []struct{ flag, value string }{{"--alias", aliases}, {"--serving-domain", domains}} {
				if strings.TrimSpace(item.value) == "" {
					args = append(args, item.flag, "")
					continue
				}
				for _, d := range strings.Split(item.value, ",") {
					args = append(args, item.flag, strings.TrimSpace(d))
				}
			}
			return args
		}
	case "embedding":
		paths, origins := strings.Join(app.Embedding.Paths, ", "), strings.Join(app.Embedding.Origins, ", ")
		groups = []*huh.Group{
			huh.NewGroup(textInput("Embeddable paths", "Comma-separated paths allowed in external iframes. Clear to disable. Use a trailing /* for a prefix.", "/banner.html, /ads/*", &paths).Validate(func(value string) error {
				e := config.Embedding{Paths: embeddingList(value)}
				if len(e.Paths) > 0 {
					e.Origins = []string{"*"}
				}
				return e.Validate()
			})),
			huh.NewGroup(textInput("Allowed embedding origins", "Comma-separated origins. Use * for any website or self for this app. Ignored when paths are empty.", "*", &origins).Validate(func(value string) error {
				return (config.Embedding{Paths: embeddingList(paths), Origins: embeddingList(value)}).Validate()
			})).WithHideFunc(func() bool { return strings.TrimSpace(paths) == "" }),
		}
		flags = func() []string {
			if strings.TrimSpace(paths) == "" {
				return []string{"--embed-path", "", "--embed-origin", ""}
			}
			var args []string
			for _, item := range []struct{ flag, value string }{{"--embed-path", paths}, {"--embed-origin", origins}} {
				for _, value := range strings.Split(item.value, ",") {
					args = append(args, item.flag, strings.TrimSpace(value))
				}
			}
			return args
		}
	case "health":
		health := app.HealthCheck
		groups = []*huh.Group{huh.NewGroup(textInput("Health check URL", "Check this URL after deployment. Clear to disable this check.", "https://example.com/up", &health))}
		flags = func() []string { return []string{"--health-check", health} }
	case "build-order":
		order := app.BuildOrder
		if order == "" {
			order = config.BuildFrontendFirst
		}
		groups = []*huh.Group{huh.NewGroup(buildOrderSelect(&order))}
		flags = func() []string { return []string{"--build-order", order} }
	case "workers":
		driver := app.Web.Driver
		if driver == "" {
			driver = "fpm"
		}
		workers := strconv.Itoa(app.Web.Workers)
		if app.Web.Workers == 0 {
			workers = "2"
		}
		queue := strconv.Itoa(app.Queue.Workers)
		groups = []*huh.Group{
			huh.NewGroup(huh.NewSelect[string]().Title("Laravel web server").Description("Choose the server your project supports.\nExample: PHP-FPM for a standard Laravel app.").Options(huh.NewOption("PHP-FPM", "fpm"), huh.NewOption("Octane / RoadRunner", "octane")).Value(&driver)),
			huh.NewGroup(textInput("Octane workers", "Processes serving web requests. Start with 2.", "2", &workers).Validate(workerCount)).WithHideFunc(func() bool { return driver != "octane" }),
			huh.NewGroup(textInput("Queue workers", "Processes for background jobs. Use 0 to disable the queue.", "1", &queue).Validate(workerCount)),
		}
		flags = func() []string {
			args := []string{"--web-driver", driver, "--queue-workers", queue}
			if driver == "octane" {
				args = append(args, "--octane-workers", workers)
			}
			return args
		}
	case "components":
		var selected []string
		options := []huh.Option[string]{}
		for _, c := range []struct {
			name, label string
			enabled     bool
		}{
			{"database", "MySQL database", app.Database.Enabled}, {"scheduler", "Scheduler", app.Scheduler.Enabled},
			{"nightwatch", "Nightwatch", app.Nightwatch.Enabled}, {"inertia-ssr", "Inertia SSR", app.InertiaSSR.Enabled},
		} {
			options = append(options, huh.NewOption(c.label, c.name).Selected(c.enabled))
			if c.enabled {
				selected = append(selected, c.name)
			}
		}
		groups = []*huh.Group{huh.NewGroup(huh.NewMultiSelect[string]().Height(4).Title("Laravel components").Description("Select the components your app uses. Disabling MySQL retains its data.\nExample: database and scheduler for scheduled jobs.").Options(options...).Value(&selected))}
		flags = func() []string {
			var args []string
			for _, c := range []string{"database", "scheduler", "nightwatch", "inertia-ssr"} {
				args = append(args, fmt.Sprintf("--%s=%t", c, slices.Contains(selected, c)))
			}
			return args
		}
	default:
		return m.editMenu(app)
	}
	return m.setForm("form", app.Name+" / "+section, func() tea.Cmd {
		args := append([]string{"edit", app.Name}, flags()...)
		return m.review(action{title: "Save settings: " + app.Name, args: args, note: "Save these settings and reserve any required ports. Choose Deploy afterward to apply them.\n\n" + clean(strings.Join(flags(), " "))})
	}, groups...)
}

func buildOrderSelect(order *string) *huh.Select[string] {
	return huh.NewSelect[string]().Title("Laravel build order").Description("Choose whether assets or PHP dependencies come first. npm ci runs before both.\nExample: Composer first for Wayfinder builds that run Artisan.").Options(
		huh.NewOption("Frontend first (default) · build assets before Composer", config.BuildFrontendFirst),
		huh.NewOption("Composer first · install PHP dependencies before build", config.BuildComposerFirst),
	).Value(order)
}

func embeddingList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	values := strings.Split(value, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values
}
