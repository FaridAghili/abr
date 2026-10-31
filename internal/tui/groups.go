package tui

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"abr/internal/config"
)

// Groups name related apps that are deployed one at a time in their order.

func (m *model) groupsMenu() tea.Cmd {
	m.menu, m.back = m.groupsMenu, m.home
	m.notice, m.context = "", ""
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.notice = "No configuration yet. Choose Add application from the main menu."
		} else {
			m.notice = clean(err.Error())
		}
	}
	rows := make([][]tableCell, len(c.Groups))
	for i, g := range c.Groups {
		rows[i] = []tableCell{cellTone(g.Name, toneName), cellText(fmt.Sprint(len(g.Apps))), deployOrder(g)}
	}
	header, keys := m.tableOptions(m.selectIndent(), []string{"GROUP", "APPS", "DEPLOY ORDER"}, rows)
	choices := make([]huh.Option[string], 0, len(c.Groups)+2)
	for i, g := range c.Groups {
		choices = append(choices, huh.NewOption(keys[i], "group:"+g.Name))
	}
	choices = append(choices, huh.NewOption("Create group", "create"), huh.NewOption("Back", "back"))
	description := "Choose a group to deploy or edit; type to filter.\n" + header
	if len(c.Groups) == 0 {
		description = "Group related apps to deploy them one by one, in your order.\nExample: api first, then the admin and shop that call it."
	}
	selected := m.choice("Groups")
	return m.setForm("apps", "Groups", func() tea.Cmd {
		switch {
		case selected == "create":
			return m.createGroupForm(nil)
		case strings.HasPrefix(selected, "group:"):
			return m.groupDestination(strings.TrimPrefix(selected, "group:"), m.groupMenu)()
		}
		return m.goBack()
	}, huh.NewGroup(huh.NewSelect[string]().Title(fmt.Sprintf("%d groups", len(c.Groups))).Description(description).Options(choices...).Value(&selected)))
}

func deployOrder(g config.Group) tableCell {
	if len(g.Apps) == 0 {
		return cellTone("no apps", toneMuted)
	}
	return cellText(strings.Join(g.Apps, " → "))
}

// groupDestination reloads a group so saved changes are visible; a deleted
// group returns to the list.
func (m *model) groupDestination(name string, open func(config.Group) tea.Cmd) func() tea.Cmd {
	return func() tea.Cmd {
		c, err := config.Load(m.options.ConfigPath)
		if err != nil {
			cmd := m.groupsMenu()
			m.notice = clean(err.Error())
			return cmd
		}
		if g, err := c.Group(name); err == nil {
			return open(g)
		}
		return m.groupsMenu()
	}
}

// savedGroup opens a group's menu after a change, saying what to do next.
func (m *model) savedGroup(name, notice string) func() tea.Cmd {
	return func() tea.Cmd {
		cmd := m.groupDestination(name, m.groupMenu)()
		m.notice = notice
		return cmd
	}
}

func (m *model) groupMenu(g config.Group) tea.Cmd {
	m.menu, m.back = m.groupDestination(g.Name, m.groupMenu), m.groupsMenu
	m.notice = ""
	m.context = "Deploy order: " + strings.Join(g.Apps, " → ")
	var choices []huh.Option[string]
	if len(g.Apps) == 0 {
		m.context = "No apps in this group. Choose Edit apps to add some."
	} else {
		choices = append(choices,
			huh.NewOption("Deploy group", "deploy"),
			huh.NewOption("Restart services", "restart"),
			huh.NewOption("Service status", "status"),
			huh.NewOption("Reorder apps", "order"))
	}
	choices = append(choices,
		huh.NewOption("Edit apps", "apps"),
		huh.NewOption("Rename group", "rename"),
		huh.NewOption("Delete group", "delete"),
		huh.NewOption("Back", "back"))
	title := "Group " + g.Name
	selected := m.choice(title)
	return m.setForm("app", title, func() tea.Cmd {
		switch selected {
		case "deploy":
			return m.groupDeployForm(g)
		case "restart":
			return m.review(action{title: "Restart group " + g.Name, args: []string{"restart", "--group", g.Name}, note: fmt.Sprintf("Order: %s\n\nRestart all services of each app in this order. Visitors may see brief errors while an app restarts. A failed app is reported and the others still restart.", strings.Join(g.Apps, " → "))})
		case "status":
			return m.start(action{title: "Group " + g.Name + " / status", args: []string{"status", "--group", g.Name}})
		case "order":
			return m.reorderGroup(g, g.Apps, g.Apps[0])
		case "apps":
			return m.groupAppsForm(g)
		case "rename":
			return m.renameGroupForm(g)
		case "delete":
			return m.review(action{title: "Delete group " + g.Name, args: []string{"group", "delete", g.Name}, note: fmt.Sprintf("Apps: %s\n\nDelete only this group. Its apps, their files, databases and services are kept and keep running.", strings.Join(g.Apps, ", ")), after: func() tea.Cmd {
				cmd := m.groupsMenu()
				m.notice = "Deleted group " + g.Name + "; its apps are kept."
				return cmd
			}})
		}
		return m.goBack()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Group actions").Options(choices...).Value(&selected)))
}

func (m *model) groupDeployForm(g config.Group) tea.Cmd {
	var noPull bool
	title := "Deploy group " + g.Name
	return m.setForm("form", title, func() tea.Cmd {
		args := []string{"deploy", "--group", g.Name}
		source := "pull the latest commits"
		if noPull {
			args = append(args, "--no-pull")
			source = "redeploy the current checkout"
		}
		note := fmt.Sprintf("Order: %s\nSource: %s\n\nDeploy one app at a time in this order. If one fails, the rest are skipped and listed; apps already deployed stay deployed. Each app has downtime while dependencies are installed, assets are built and services restart; changes are not rolled back.", strings.Join(g.Apps, " → "), source)
		if c, err := config.Load(m.options.ConfigPath); err == nil && slices.ContainsFunc(c.Apps, func(a config.App) bool { return a.Type == "laravel" && slices.Contains(g.Apps, a.Name) }) {
			note += " Laravel deployments also run database migrations."
		}
		return m.review(action{title: title, args: args, note: note + "\n\nA summary lists each app's result at the end."})
	}, huh.NewGroup(huh.NewSelect[bool]().Title("Source code").Description("Pull your latest commits, or redeploy the code already on the server.\nExample: current checkout to rebuild after editing .env.").Options(huh.NewOption("Pull latest commits", false), huh.NewOption("Redeploy current checkout", true)).Value(&noPull)))
}

// groupAppChoices lists every app with the groups it is already in.
func (m *model) groupAppChoices() ([]huh.Option[string], string, error) {
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	if len(c.Apps) == 0 {
		return nil, "", errors.New("No apps registered. Choose Add application from the main menu.")
	}
	slices.SortFunc(c.Apps, func(a, b config.App) int { return strings.Compare(a.Name, b.Name) })
	rows := make([][]tableCell, len(c.Apps))
	for i, app := range c.Apps {
		rows[i] = []tableCell{cellTone(app.Name, toneName), appGroups(c, app.Name), cellTone(app.Domain, toneInfo)}
	}
	header, keys := m.tableOptions(m.multiSelectIndent(), []string{"APP", "GROUPS", "DOMAIN"}, rows)
	choices := make([]huh.Option[string], len(c.Apps))
	for i, app := range c.Apps {
		choices[i] = huh.NewOption(keys[i], app.Name)
	}
	return choices, header, nil
}

func appGroups(c config.Config, app string) tableCell {
	if groups := c.GroupsOf(app); len(groups) > 0 {
		return cellText(strings.Join(groups, ", "))
	}
	return cellTone("—", toneMuted)
}

func (m *model) createGroupForm(apps []string) tea.Cmd {
	choices, header, err := m.groupAppChoices()
	if err != nil {
		return m.workflowError("Create group", err)
	}
	c, _ := config.Load(m.options.ConfigPath)
	var name string
	return m.setForm("form", "Create group", func() tea.Cmd {
		return m.review(action{title: "Create group " + name, args: append([]string{"group", "create", name}, apps...), note: fmt.Sprintf("Group: %s\nOrder: %s\n\nSave this group. Apps are not changed or deployed.", name, strings.Join(apps, " → ")), after: m.savedGroup(name, "Created group "+name+". Choose Reorder apps to set the deploy order, then Deploy group.")})
	}, huh.NewGroup(textInput("Group name", "Lowercase letters, digits and hyphens.", "shop", &name).Validate(func(s string) error {
		if err := applicationName(s); err != nil {
			return err
		}
		if _, err := c.Group(s); err == nil {
			return errors.New("A group with this name exists")
		}
		return nil
	})),
		huh.NewGroup(selectApps("Apps in this group", "Space selects an app, Ctrl+A selects all; reorder them next.\nExample: select api, admin and shop.", header, choices, &apps)))
}

func (m *model) groupAppsForm(g config.Group) tea.Cmd {
	choices, header, err := m.groupAppChoices()
	if err != nil {
		return m.workflowError("Edit apps: "+g.Name, err)
	}
	selected := slices.Clone(g.Apps)
	return m.setForm("form", "Edit apps: "+g.Name, func() tea.Cmd {
		// Kept apps keep their places; added apps deploy last.
		apps := slices.DeleteFunc(slices.Clone(g.Apps), func(name string) bool { return !slices.Contains(selected, name) })
		for _, name := range selected {
			if !slices.Contains(apps, name) {
				apps = append(apps, name)
			}
		}
		return m.review(action{title: "Save group " + g.Name, args: append([]string{"group", "apps", g.Name}, apps...), note: fmt.Sprintf("Order: %s\n\nAdded apps deploy last. Apps left out stay registered and running.", strings.Join(apps, " → ")), after: m.savedGroup(g.Name, "Saved apps. Choose Reorder apps to change the deploy order.")})
	}, huh.NewGroup(selectApps("Apps in "+g.Name, "Space adds or removes an app; added apps deploy last.\nExample: add worker to deploy it after the others.", header, choices, &selected)))
}

// reorderGroup changes the order in memory, one move at a time, and saves it
// once at the end.
func (m *model) reorderGroup(g config.Group, order []string, focus string) tea.Cmd {
	choices := make([]huh.Option[string], 0, len(order)+2)
	for i, name := range order {
		choices = append(choices, huh.NewOption(fmt.Sprintf("%d. %s", i+1, name), name))
	}
	choices = append(choices, huh.NewOption("Save order", "save"), huh.NewOption("Back", "back"))
	selected := focus
	return m.setForm("form", "Reorder "+g.Name, func() tea.Cmd {
		switch selected {
		case "save":
			if slices.Equal(order, g.Apps) {
				cmd := m.groupDestination(g.Name, m.groupMenu)()
				m.notice = "Order unchanged."
				return cmd
			}
			return m.review(action{title: "Save order: " + g.Name, args: append([]string{"group", "apps", g.Name}, order...), note: fmt.Sprintf("Order: %s\n\nSave this deploy order. Nothing is deployed now.", strings.Join(order, " → ")), after: m.savedGroup(g.Name, "Saved the order. Choose Deploy group to deploy the apps in it.")})
		case "back":
			return m.goBack()
		}
		return m.moveAppForm(g, order, selected)
	}, huh.NewGroup(huh.NewSelect[string]().Title("Deploy order").Description("Apps deploy top to bottom. Choose an app to move it, then Save order.\nExample: move api to the top so apps calling it deploy after it.").Options(choices...).Value(&selected)))
}

func (m *model) moveAppForm(g config.Group, order []string, name string) tea.Cmd {
	at := slices.Index(order, name)
	rest := slices.Delete(slices.Clone(order), at, at+1)
	choices := []huh.Option[int]{huh.NewOption("First", 0)}
	for i, other := range rest {
		choices = append(choices, huh.NewOption("After "+other, i+1))
	}
	position := at
	cmd := m.setForm("form", "Reorder "+g.Name, func() tea.Cmd {
		return m.reorderGroup(g, slices.Insert(rest, position, name), name)
	}, huh.NewGroup(huh.NewSelect[int]().Title("Move "+name).Description("Choose where "+name+" deploys in this group.\nExample: First deploys it before the others.").Options(choices...).Value(&position)))
	m.back = func() tea.Cmd { return m.reorderGroup(g, order, name) }
	return cmd
}

func (m *model) renameGroupForm(g config.Group) tea.Cmd {
	c, _ := config.Load(m.options.ConfigPath)
	name := g.Name
	return m.setForm("form", "Rename group "+g.Name, func() tea.Cmd {
		if name == g.Name {
			return m.goBack()
		}
		return m.review(action{title: "Rename group " + g.Name, args: []string{"group", "rename", g.Name, name}, note: fmt.Sprintf("Rename group %s to %s. Its apps and their order are kept.", g.Name, name), after: m.savedGroup(name, "Renamed group to "+name+".")})
	}, huh.NewGroup(textInput("New name", "Lowercase letters, digits and hyphens. Apps are not changed.", "storefront", &name).Validate(func(s string) error {
		if err := applicationName(s); err != nil {
			return err
		}
		if _, err := c.Group(s); err == nil && s != g.Name {
			return errors.New("A group with this name exists")
		}
		return nil
	})))
}

// appGroupsForm sets the groups one app belongs to. Without groups yet, it
// starts a new group with this app selected.
func (m *model) appGroupsForm(app config.App) tea.Cmd {
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		return m.workflowError("Groups: "+app.Name, err)
	}
	if len(c.Groups) == 0 {
		cmd := m.createGroupForm([]string{app.Name})
		m.notice = "No groups yet. Create one; " + app.Name + " is selected."
		return cmd
	}
	rows := make([][]tableCell, len(c.Groups))
	for i, g := range c.Groups {
		rows[i] = []tableCell{cellTone(g.Name, toneName), deployOrder(g)}
	}
	header, keys := m.tableOptions(m.multiSelectIndent(), []string{"GROUP", "DEPLOY ORDER"}, rows)
	choices := make([]huh.Option[string], len(c.Groups))
	for i, g := range c.Groups {
		choices[i] = huh.NewOption(keys[i], g.Name)
	}
	selected := c.GroupsOf(app.Name)
	return m.setForm("form", "Groups: "+app.Name, func() tea.Cmd {
		groups := "none"
		if len(selected) > 0 {
			groups = strings.Join(selected, ", ")
		}
		return m.review(action{title: "Save groups: " + app.Name, args: append([]string{"group", "assign", app.Name}, selected...), note: fmt.Sprintf("Groups: %s\n\n%s keeps its place in groups it stays in and deploys last in groups it joins. Nothing is deployed now.", groups, app.Name)})
	}, huh.NewGroup(huh.NewMultiSelect[string]().Height(min(len(choices)+2, 12)).Title("Groups for "+app.Name).Description("Space adds or removes a group; select none to leave all groups.\nExample: add api to shop to deploy it with the shop's apps.\n"+header).Options(choices...).Value(&selected)))
}
