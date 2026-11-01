package manager

import (
	"os"
	"reflect"
	"testing"

	"abr/internal/config"
)

func TestGroupsKeepAppsAndOrder(t *testing.T) {
	m := testManager(t)
	for _, name := range []string{"api", "admin", "shop"} {
		if _, err := m.Register(testApp(name), nil); err != nil {
			t.Fatal(err)
		}
	}
	apps := func(name string) []string {
		c, err := config.Load(m.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		g, err := c.Group(name)
		if err != nil {
			t.Fatal(err)
		}
		return g.Apps
	}
	before, _ := os.ReadFile(m.ConfigPath)
	if err := m.CreateGroup("store", []string{"shop", "api"}, false); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(m.ConfigPath); string(after) != string(before) {
		t.Fatal("preview saved the group")
	}
	if err := m.CreateGroup("store", []string{"shop", "api"}, true); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		m.CreateGroup("store", []string{"api"}, true),
		m.CreateGroup("other", nil, true),
		m.CreateGroup("other", []string{"missing"}, true),
		m.SetGroupApps("store", nil, true),
		m.SetGroupApps("missing", []string{"api"}, true),
		m.RenameGroup("missing", "other", true),
		m.AssignGroups("api", []string{"missing"}, true),
	} {
		if err == nil {
			t.Fatal("invalid group change accepted")
		}
	}
	if err := m.SetGroupApps("store", []string{"api", "admin", "shop"}, true); err != nil || !reflect.DeepEqual(apps("store"), []string{"api", "admin", "shop"}) {
		t.Fatal("order not saved", err)
	}
	if err := m.CreateGroup("backend", []string{"api"}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.RenameGroup("backend", "store", true); err == nil {
		t.Fatal("rename onto another group accepted")
	}
	// Leaving one group and joining another keeps places in the rest.
	if err := m.AssignGroups("admin", []string{"backend"}, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(apps("store"), []string{"api", "shop"}) || !reflect.DeepEqual(apps("backend"), []string{"api", "admin"}) {
		t.Fatal("assignment changed the wrong groups", apps("store"), apps("backend"))
	}
	if err := m.RenameGroup("store", "shopfront", true); err != nil || !reflect.DeepEqual(apps("shopfront"), []string{"api", "shop"}) {
		t.Fatal("rename lost apps", err)
	}
	// Removing an app keeps its groups; deleting a group keeps its apps.
	if err := m.Remove("api", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(apps("shopfront"), []string{"shop"}) || !reflect.DeepEqual(apps("backend"), []string{"admin"}) {
		t.Fatal("removed app left in groups")
	}
	if err := m.DeleteGroup("shopfront", true); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(m.ConfigPath)
	if err != nil || len(c.Groups) != 1 || len(c.Apps) != 2 {
		t.Fatal("delete removed more than the group", c.Groups, len(c.Apps), err)
	}
}
