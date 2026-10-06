// Package manager coordinates configuration and registry files. Linux service
// lifecycle and provisioning operations intentionally do not exist here.
package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sites-manager/internal/config"
	"sites-manager/internal/ports"
	"sites-manager/internal/storage"
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
	var result ports.Registry
	err := m.locked(func() error {
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
		// Two files cannot be atomically renamed together. Save reservations first:
		// a crash or config write failure may retain safe, reusable reservations.
		if err := r.Save(m.RegistryPath()); err != nil {
			return fmt.Errorf("save registry: %w", err)
		}
		if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
			return fmt.Errorf("save config (port reservations retained; retry registration): %w", err)
		}
		result = r
		return nil
	})
	return result, err
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

// Doctor is limited to portable config/state/port checks. It cannot identify
// which process owns a listener and does not inspect systemd, Caddy, or PHP.
func (m Manager) Doctor() error {
	c, r, err := m.Snapshot()
	if err != nil {
		return err
	}
	var issues []error
	names := map[string]bool{}
	for _, a := range c.Apps {
		names[a.Name] = true
		for _, p := range a.Endpoints() {
			port, ok := r.Lookup(a.Name, p)
			if !ok {
				issues = append(issues, fmt.Errorf("%s/%s has no reservation; run sites ports --allocate", a.Name, p))
				continue
			}
			if err := m.probe()(port); err != nil {
				issues = append(issues, fmt.Errorf("%s/%s at %d: %w", a.Name, p, port, err))
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
