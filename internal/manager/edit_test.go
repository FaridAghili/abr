package manager

import (
	"bytes"
	"os"
	"reflect"
	"sync"
	"testing"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/storage"
)

func TestConcurrentEditsPreserveUnspecifiedSettingsAndPorts(t *testing.T) {
	m := testManager(t)
	a := testApp("app")
	a.Scheduler.Enabled = true
	old, err := m.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes := []config.AppChanges{
		{Values: config.App{Queue: config.Queue{Workers: 2}}, Fields: []string{"queue-workers"}},
		{Values: config.App{Nightwatch: config.Component{Enabled: true}}, Fields: []string{"nightwatch"}},
		{Fields: []string{"scheduler"}},
	}
	var wg sync.WaitGroup
	for _, change := range changes {
		wg.Go(func() {
			if err := m.Edit(a.Name, change, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	c, r, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got := c.Apps[0]
	if got.Queue.Workers != 2 || !got.Nightwatch.Enabled || got.Scheduler.Enabled || got.Domain != a.Domain || got.User != a.User || got.Directory != a.Directory {
		t.Fatalf("lost settings: %+v", got)
	}
	for _, p := range old.Assignments {
		if port, _ := r.Lookup(p.App, p.Purpose); port != p.Port {
			t.Fatal("changed existing reservation")
		}
	}
	if len(r.Assignments) != len(old.Assignments)+1 {
		t.Fatal("missing Nightwatch reservation")
	}
	if err := m.Edit(a.Name, config.AppChanges{Fields: []string{"nightwatch"}}, true); err != nil {
		t.Fatal(err)
	}
	_, after, _ := m.Snapshot()
	if !reflect.DeepEqual(r, after) {
		t.Fatal("disabled endpoint lost reservation")
	}
}

func TestEditRejectsDomainAndPortConflictsWithoutSaving(t *testing.T) {
	m := testManager(t)
	if _, err := m.Register(testApp("app"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(testApp("other"), nil); err != nil {
		t.Fatal(err)
	}
	c, _, _ := m.Snapshot()
	c.Ports.Last = c.Ports.First + 4
	data, _ := config.Encode(c)
	if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
		t.Fatal(err)
	}
	m.Probe = func(int) error { return ports.ErrOccupied }
	beforeConfig, _ := os.ReadFile(m.ConfigPath)
	beforePorts, _ := os.ReadFile(m.RegistryPath())
	for _, change := range []config.AppChanges{
		{Values: config.App{Domain: "other.test"}, Fields: []string{"domain"}},
		{Values: config.App{Nightwatch: config.Component{Enabled: true}}, Fields: []string{"nightwatch"}},
	} {
		if err := m.Edit("app", change, true); err == nil {
			t.Fatal("conflict succeeded")
		}
		gotConfig, _ := os.ReadFile(m.ConfigPath)
		gotPorts, _ := os.ReadFile(m.RegistryPath())
		if !bytes.Equal(beforeConfig, gotConfig) || !bytes.Equal(beforePorts, gotPorts) {
			t.Fatal("failed edit changed files")
		}
	}
	if err := m.Edit("missing", config.AppChanges{}, true); err == nil {
		t.Fatal("unknown app edited")
	}
	if err := m.Edit("app", config.AppChanges{Fields: []string{"user"}}, true); err == nil {
		t.Fatal("identity edited")
	}
}

func TestEditPreviewDoesNotCreateStateOrLocks(t *testing.T) {
	m := testManager(t)
	a := testApp("app")
	data, _ := config.Encode(config.Config{Ports: config.Default().Ports, Apps: []config.App{a}})
	if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
		t.Fatal(err)
	}
	change := config.AppChanges{Values: config.App{Domain: "changed.test"}, Fields: []string{"domain"}}
	if err := m.Edit("app", change, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.StateDir); !os.IsNotExist(err) {
		t.Fatal("preview created state")
	}
	if _, err := os.Stat(m.ConfigPath + ".lock"); !os.IsNotExist(err) {
		t.Fatal("preview created lock")
	}
	after, _ := os.ReadFile(m.ConfigPath)
	if !bytes.Equal(data, after) {
		t.Fatal("preview saved config")
	}
}
