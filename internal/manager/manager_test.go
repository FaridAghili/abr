package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/storage"
)

func testManager(t *testing.T) Manager {
	t.Helper()
	dir := t.TempDir()
	return Manager{ConfigPath: filepath.Join(dir, "config", "config.toml"), StateDir: filepath.Join(dir, "state"), Probe: func(int) error { return nil }}
}

func testApp(name string) config.App {
	return config.App{Name: name, Directory: "/srv/" + name, User: name, Type: "laravel", Domain: name + ".test", Web: config.Web{Driver: "octane"}}
}

func TestConcurrentRegistrations(t *testing.T) {
	m := testManager(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.Register(testApp(fmt.Sprintf("app%d", i)), nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	c, r, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Apps) != 8 || len(r.Assignments) != 16 {
		t.Fatalf("lost registration: %d apps, %d ports", len(c.Apps), len(r.Assignments))
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	beforeConfig, _ := os.ReadFile(m.ConfigPath)
	beforeState, _ := os.ReadFile(m.RegistryPath())
	if _, err := m.Register(c.Apps[0], nil); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	afterConfig, _ := os.ReadFile(m.ConfigPath)
	afterState, _ := os.ReadFile(m.RegistryPath())
	if string(beforeConfig) != string(afterConfig) || string(beforeState) != string(afterState) {
		t.Fatal("duplicate registration changed files")
	}
	after, err := m.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, after) {
		t.Fatal("allocation changed saved assignments")
	}
}

func TestConfigurationEditsRetainPorts(t *testing.T) {
	m := testManager(t)
	a := testApp("app")
	first, err := m.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Apps[0].Web.Driver = "fpm"
	c.Apps[0].Nightwatch.Enabled = true
	data, err := config.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AtomicWrite(m.ConfigPath, data); err != nil {
		t.Fatal(err)
	}
	if err := m.Doctor(); err == nil || !strings.Contains(err.Error(), "no reservation") {
		t.Fatalf("got %v", err)
	}
	second, err := m.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Assignments) != 3 {
		t.Fatalf("got %+v", second)
	}
	for _, old := range first.Assignments {
		if got, _ := second.Lookup(old.App, old.Purpose); got != old.Port {
			t.Fatal("disabled reservation lost")
		}
	}
	if err := m.Doctor(); err != nil {
		t.Fatal(err)
	}
	m.Probe = func(int) error { return ports.ErrOccupied }
	if err := m.Doctor(); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("got %v", err)
	}
}

func TestFailurePreservesFiles(t *testing.T) {
	m := testManager(t)
	if _, err := m.Register(testApp("existing"), nil); err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(m.ConfigPath)
	bad := []byte(`{"version":1,"assignments":null}`)
	if err := os.WriteFile(m.RegistryPath(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(testApp("new"), nil); err == nil {
		t.Fatal("accepted corrupt state")
	}
	if _, err := m.Allocate(); err == nil {
		t.Fatal("allocation accepted corrupt state")
	}
	configAfter, _ := os.ReadFile(m.ConfigPath)
	stateAfter, _ := os.ReadFile(m.RegistryPath())
	if string(configBefore) != string(configAfter) || string(bad) != string(stateAfter) {
		t.Fatal("failed operation changed files")
	}
}

func TestInterruptedRegistrationRecovery(t *testing.T) {
	m := testManager(t)
	a := testApp("app")
	r := ports.Empty()
	if err := r.Ensure(a, config.Default().Ports, nil, m.Probe); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(m.RegistryPath()); err != nil {
		t.Fatal(err)
	}
	saved, err := m.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, r) {
		t.Fatal("retry did not reuse retained reservations")
	}
}
