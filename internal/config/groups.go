package config

import (
	"fmt"
	"slices"
)

// Group lists related apps in the order they are deployed. Groups only refer
// to apps by name; deleting a group leaves its apps unchanged.
type Group struct {
	Name string   `toml:"name"`
	Apps []string `toml:"apps"`
}

func (c Config) validateGroups(apps map[string]bool) error {
	seen := make(map[string]bool, len(c.Groups))
	for _, g := range c.Groups {
		if !ValidName(g.Name) {
			return fmt.Errorf("group %q: name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens (max 63 characters)", g.Name)
		}
		if seen[g.Name] {
			return fmt.Errorf("duplicate group name %q", g.Name)
		}
		seen[g.Name] = true
		members := make(map[string]bool, len(g.Apps))
		for _, name := range g.Apps {
			if !apps[name] {
				return fmt.Errorf("group %q: unknown application %q", g.Name, name)
			}
			if members[name] {
				return fmt.Errorf("group %q lists %q more than once", g.Name, name)
			}
			members[name] = true
		}
	}
	return nil
}

// Group returns the named group.
func (c Config) Group(name string) (Group, error) {
	i := slices.IndexFunc(c.Groups, func(g Group) bool { return g.Name == name })
	if i < 0 {
		return Group{}, fmt.Errorf("unknown group %q", name)
	}
	return c.Groups[i], nil
}

// GroupsOf names the groups that include app, in configuration order.
func (c Config) GroupsOf(app string) []string {
	var names []string
	for _, g := range c.Groups {
		if slices.Contains(g.Apps, app) {
			names = append(names, g.Name)
		}
	}
	return names
}
