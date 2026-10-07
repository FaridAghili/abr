package manager

import (
	"fmt"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/storage"
)

func (m Manager) Edit(name string, changes config.AppChanges, save bool) error {
	fn := func() error {
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		found := false
		for i, app := range c.Apps {
			if app.Name == name {
				c.Apps[i], err = changes.Apply(app)
				if err != nil {
					return err
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown application %q", name)
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
