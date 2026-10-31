package manager

import (
	"fmt"
	"slices"

	"abr/internal/config"
	"abr/internal/storage"
)

// CreateGroup adds a group deploying apps in the given order.
func (m Manager) CreateGroup(name string, apps []string, save bool) error {
	return m.editGroups(save, func(c *config.Config) error {
		if _, err := c.Group(name); err == nil {
			return fmt.Errorf("group %q already exists", name)
		}
		if len(apps) == 0 {
			return fmt.Errorf("select at least one app for group %q", name)
		}
		c.Groups = append(c.Groups, config.Group{Name: name, Apps: apps})
		return nil
	})
}

// SetGroupApps replaces a group's apps; their order is the deploy order.
func (m Manager) SetGroupApps(name string, apps []string, save bool) error {
	return m.editGroups(save, func(c *config.Config) error {
		i, err := groupIndex(*c, name)
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			return fmt.Errorf("select at least one app, or delete group %q", name)
		}
		c.Groups[i].Apps = apps
		return nil
	})
}

func (m Manager) RenameGroup(name, newName string, save bool) error {
	return m.editGroups(save, func(c *config.Config) error {
		i, err := groupIndex(*c, name)
		if err != nil {
			return err
		}
		if _, err := c.Group(newName); err == nil && newName != name {
			return fmt.Errorf("group %q already exists", newName)
		}
		c.Groups[i].Name = newName
		return nil
	})
}

// DeleteGroup removes only the group; its apps are kept.
func (m Manager) DeleteGroup(name string, save bool) error {
	return m.editGroups(save, func(c *config.Config) error {
		i, err := groupIndex(*c, name)
		if err != nil {
			return err
		}
		c.Groups = slices.Delete(c.Groups, i, i+1)
		return nil
	})
}

// AssignGroups makes app a member of exactly the named groups. It keeps its
// place in groups it stays in and is deployed last in groups it joins.
func (m Manager) AssignGroups(app string, groups []string, save bool) error {
	return m.editGroups(save, func(c *config.Config) error {
		if !slices.ContainsFunc(c.Apps, func(a config.App) bool { return a.Name == app }) {
			return fmt.Errorf("unknown application %q", app)
		}
		for _, name := range groups {
			if _, err := c.Group(name); err != nil {
				return err
			}
		}
		for i, g := range c.Groups {
			member, wanted := slices.Contains(g.Apps, app), slices.Contains(groups, g.Name)
			switch {
			case wanted && !member:
				c.Groups[i].Apps = append(g.Apps, app)
			case !wanted && member:
				c.Groups[i].Apps = slices.DeleteFunc(slices.Clone(g.Apps), func(name string) bool { return name == app })
			}
		}
		return nil
	})
}

func groupIndex(c config.Config, name string) (int, error) {
	i := slices.IndexFunc(c.Groups, func(g config.Group) bool { return g.Name == name })
	if i < 0 {
		return 0, fmt.Errorf("unknown group %q", name)
	}
	return i, nil
}

// editGroups validates fn's change and, when saving, writes it. Groups only
// name apps, so port reservations and host state are untouched.
func (m Manager) editGroups(save bool, fn func(*config.Config) error) error {
	edit := func() error {
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		if err := fn(&c); err != nil {
			return err
		}
		data, err := config.Encode(c)
		if err != nil {
			return err
		}
		if !save {
			return nil
		}
		return storage.AtomicWrite(m.ConfigPath, data)
	}
	if save {
		return m.locked(edit)
	}
	return edit()
}
