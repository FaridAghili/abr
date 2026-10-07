// Package ports manages reservations. It does not start services or hold listeners open.
package ports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"abr/internal/config"
	"abr/internal/storage"
)

type Assignment struct {
	App     string `json:"app"`
	Purpose string `json:"purpose"`
	Port    int    `json:"port"`
}

type Registry struct {
	Version     int          `json:"version"`
	Assignments []Assignment `json:"assignments"`
}

var ErrOccupied = errors.New("port is occupied")

type Probe func(int) error

func ValidPurpose(p string) bool {
	switch p {
	case "octane-http", "roadrunner-rpc", "nuxt-http", "inertia-ssr", "nightwatch-ingest":
		return true
	}
	return false
}

func Empty() Registry { return Registry{Version: 1, Assignments: []Assignment{}} }

func Load(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Empty(), nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return Registry{}, fmt.Errorf("corrupt registry %s: %w", path, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Registry{}, fmt.Errorf("corrupt registry %s: trailing JSON", path)
	}
	if err := r.Validate(); err != nil {
		return Registry{}, fmt.Errorf("corrupt registry %s: %w", path, err)
	}
	return r, nil
}

func (r Registry) Validate() error {
	if r.Version != 1 || r.Assignments == nil {
		return fmt.Errorf("expected version 1 and an assignments array")
	}
	used, keys := map[int]bool{}, map[string]bool{}
	for _, a := range r.Assignments {
		if !config.ValidName(a.App) || !ValidPurpose(a.Purpose) || a.Port < 1024 || a.Port > 65535 {
			return fmt.Errorf("invalid assignment %+v", a)
		}
		key := a.App + "/" + a.Purpose
		if keys[key] {
			return fmt.Errorf("duplicate endpoint %s", key)
		}
		if used[a.Port] {
			return fmt.Errorf("duplicate port %d", a.Port)
		}
		keys[key], used[a.Port] = true, true
	}
	return nil
}

func (r Registry) Save(path string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	r.Sort()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return storage.AtomicWrite(path, append(data, '\n'))
}

func (r *Registry) Sort() {
	sort.Slice(r.Assignments, func(i, j int) bool {
		a, b := r.Assignments[i], r.Assignments[j]
		if a.App != b.App {
			return a.App < b.App
		}
		return a.Purpose < b.Purpose
	})
}

func (r Registry) Lookup(app, purpose string) (int, bool) {
	for _, a := range r.Assignments {
		if a.App == app && a.Purpose == purpose {
			return a.Port, true
		}
	}
	return 0, false
}

// Ensure retains every saved assignment, including disabled components.
// Imports are explicit requested ports, which must be free at registration time.
// On error the receiver stays unchanged.
func (r *Registry) Ensure(app config.App, pool config.Range, imports map[string]int, probe Probe) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := app.Validate(); err != nil {
		return err
	}
	if pool.First < 1024 || pool.Last > 65535 || pool.Last < pool.First {
		return fmt.Errorf("invalid port range")
	}
	next := Registry{Version: 1, Assignments: append([]Assignment{}, r.Assignments...)}
	required := map[string]bool{}
	for _, p := range app.Endpoints() {
		required[p] = true
	}
	used := map[int]string{}
	for _, a := range next.Assignments {
		used[a.Port] = a.App + "/" + a.Purpose
	}
	// Reserve all imports before automatically allocating anything.
	purposes := make([]string, 0, len(imports))
	for p := range imports {
		purposes = append(purposes, p)
	}
	sort.Strings(purposes)
	for _, p := range purposes {
		port := imports[p]
		if !required[p] {
			return fmt.Errorf("app %s: endpoint %q is not enabled", app.Name, p)
		}
		if port < 1024 || port > 65535 {
			return fmt.Errorf("app %s: imported port must be between 1024 and 65535", app.Name)
		}
		if saved, ok := next.Lookup(app.Name, p); ok {
			if saved != port {
				return fmt.Errorf("app %s: %s already reserved at %d", app.Name, p, saved)
			}
			continue
		}
		if owner, ok := used[port]; ok {
			return fmt.Errorf("port %d already reserved for %s", port, owner)
		}
		if err := probe(port); err != nil {
			return fmt.Errorf("app %s: cannot reserve %s at %d: %w", app.Name, p, port, err)
		}
		next.Assignments = append(next.Assignments, Assignment{app.Name, p, port})
		used[port] = app.Name + "/" + p
	}
	for _, p := range app.Endpoints() {
		if _, ok := next.Lookup(app.Name, p); ok {
			continue
		}
		selected := 0
		for port := pool.First; port <= pool.Last; port++ {
			if _, ok := used[port]; ok {
				continue
			}
			err := probe(port)
			if errors.Is(err, ErrOccupied) {
				continue
			}
			if err != nil {
				return fmt.Errorf("check port %d: %w", port, err)
			}
			selected = port
			break
		}
		if selected == 0 {
			return fmt.Errorf("app %s: no available ports in %d-%d for %s", app.Name, pool.First, pool.Last, p)
		}
		next.Assignments = append(next.Assignments, Assignment{app.Name, p, selected})
		used[selected] = app.Name + "/" + p
	}
	next.Sort()
	*r = next
	return nil
}
