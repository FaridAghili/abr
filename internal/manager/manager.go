// Package manager coordinates portable configuration and registry files.
package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/storage"
)

type Manager struct {
	ConfigPath string
	StateDir   string
	Probe      ports.Probe
}

func (m Manager) RegistryPath() string { return filepath.Join(m.StateDir, "ports.json") }
func (m Manager) probe() ports.Probe {
	if m.Probe != nil {
		return m.Probe
	}
	return ports.CheckAvailable
}

func (m Manager) locked(fn func() error) error {
	// Fixed order, also protecting one config used with different state directories.
	return storage.WithLock(m.ConfigPath+".lock", func() error {
		return storage.WithLock(filepath.Join(m.StateDir, "ports.lock"), fn)
	})
}

func (m Manager) Register(app config.App, imports map[string]int) (ports.Registry, error) {
	return m.register(app, imports, true)
}

// PreviewRegister validates and allocates in memory without creating files or locks.
func (m Manager) PreviewRegister(app config.App, imports map[string]int) (ports.Registry, error) {
	return m.register(app, imports, false)
}

func (m Manager) register(app config.App, imports map[string]int, save bool) (ports.Registry, error) {
	var result ports.Registry
	fn := func() error {
		c, err := config.Load(m.ConfigPath)
		if errors.Is(err, os.ErrNotExist) {
			c, err = config.Default(), nil
		}
		if err != nil {
			return err
		}
		c.Apps = append(c.Apps, app)
		data, err := config.Encode(c) // Includes duplicate name/directory/domain checks.
		if err != nil {
			return err
		}
		r, err := ports.Load(m.RegistryPath())
		if err != nil {
			return err
		}
		// An imported port takes precedence over new automatic reservations.
		if err := r.Ensure(app, c.Ports, imports, m.probe()); err != nil {
			return err
		}
		if err := ensureAll(&r, c, m.probe()); err != nil {
			return err
		}
		result = r
		if !save {
			return nil
		}
		// Two files cannot be atomically renamed together. Save reservations first:
		// a crash or config write failure may retain safe, reusable reservations.
		if err := r.Save(m.RegistryPath()); err != nil {
			return fmt.Errorf("save registry: %w", err)
		}
		if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
			return fmt.Errorf("save config (port reservations retained; retry registration): %w", err)
		}
		return nil
	}
	var err error
	if save {
		err = m.locked(fn)
	} else {
		err = fn()
	}
	return result, err
}

// Remove must only be called after host services are confirmed stopped.
// Config is saved first; interruption can leave conservative orphan reservations.
func (m Manager) Remove(name string) error {
	return m.locked(func() error {
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		r, err := ports.Load(m.RegistryPath())
		if err != nil {
			return err
		}
		found := false
		apps := c.Apps[:0]
		for _, a := range c.Apps {
			if a.Name == name {
				found = true
			} else {
				apps = append(apps, a)
			}
		}
		if !found {
			return fmt.Errorf("unknown application %q", name)
		}
		c.Apps = apps
		data, err := config.Encode(c)
		if err != nil {
			return err
		}
		if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
			return err
		}
		assignments := r.Assignments[:0]
		for _, a := range r.Assignments {
			if a.App != name {
				assignments = append(assignments, a)
			}
		}
		r.Assignments = assignments
		return r.Save(m.RegistryPath())
	})
}

func ensureAll(r *ports.Registry, c config.Config, probe ports.Probe) error {
	apps := append([]config.App{}, c.Apps...)
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	for _, a := range apps {
		if err := r.Ensure(a, c.Ports, nil, probe); err != nil {
			return err
		}
	}
	return nil
}

// Allocate reconciles manually edited configuration without releasing reservations.
func (m Manager) Allocate() (ports.Registry, error) {
	var result ports.Registry
	err := m.locked(func() error {
		c, err := config.Load(m.ConfigPath)
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
		if err := r.Save(m.RegistryPath()); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

// Snapshot returns a consistent config/registry pair under the writer locks.
func (m Manager) Snapshot() (config.Config, ports.Registry, error) {
	var c config.Config
	var r ports.Registry
	err := m.locked(func() error {
		var err error
		c, err = config.Load(m.ConfigPath)
		if err != nil {
			return err
		}
		r, err = ports.Load(m.RegistryPath())
		return err
	})
	return c, r, err
}

// CheckReservations checks the configuration/registry relationship without
// probing live ports. Listener ownership belongs to the host diagnostic.
func CheckReservations(c config.Config, r ports.Registry) error {
	var issues []error
	names := map[string]bool{}
	for _, a := range c.Apps {
		names[a.Name] = true
		for _, p := range a.Endpoints() {
			_, ok := r.Lookup(a.Name, p)
			if !ok {
				issues = append(issues, fmt.Errorf("%s/%s has no reservation; run abr ports --allocate", a.Name, p))
			}
		}
	}
	for _, a := range r.Assignments {
		if !names[a.App] {
			issues = append(issues, fmt.Errorf("reservation %s/%s at %d has no configured app (retained)", a.App, a.Purpose, a.Port))
		}
	}
	return errors.Join(issues...)
}

// Registry reads all reservations, including ones left by an interrupted registration.
func (m Manager) Registry() (ports.Registry, error) {
	var r ports.Registry
	err := storage.WithLock(filepath.Join(m.StateDir, "ports.lock"), func() error {
		var err error
		r, err = ports.Load(m.RegistryPath())
		r.Sort()
		return err
	})
	return r, err
}
