package manager

import (
	"fmt"
	"slices"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/storage"
)

func (m Manager) Edit(name string, changes config.AppChanges, save bool) error {
	return m.EditApps([]string{name}, changes, save)
}

// EditApps applies the same changes to every named app in one configuration write.
func (m Manager) EditApps(names []string, changes config.AppChanges, save bool) error {
	fn := func() error {
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		for _, name := range names {
			i := slices.IndexFunc(c.Apps, func(a config.App) bool { return a.Name == name })
			if i < 0 {
				return fmt.Errorf("unknown application %q", name)
			}
			if c.Apps[i], err = changes.Apply(c.Apps[i]); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		data, err := config.Encode(c)
		if err != nil {
			return err
		}
		r, err := ports.Load(m.RegistryPath())
		if err != nil {
			return err
		}
		if err := ensureAll(&r, c, m.probe()); err != nil {
			return err
		}
		if !save {
			return nil
		}
		// Retain previous reservations, including endpoints disabled by this edit.
		if err := r.Save(m.RegistryPath()); err != nil {
			return err
		}
		return storage.AtomicWrite(m.ConfigPath, data)
	}
	if save {
		return m.locked(fn)
	}
	return fn()
}
