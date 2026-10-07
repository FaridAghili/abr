package host

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"abr/internal/config"
	"abr/internal/services"
)

func TestPurgeResetsOnlyFailedUnits(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		state     string
		resetFail bool
	}{
		{name: "inactive-unloaded", state: "inactive", resetFail: true},
		{name: "failed", state: "failed"},
		{name: "reset-error", state: "failed", resetFail: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, r, _, a := fixture(t)
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			unit := "abr-app-queue@1.service"
			path := "/etc/systemd/system/abr-app-queue@.service"
			if err := h.write(path, []byte(services.Marker+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Files: []string{path}, Units: []string{unit}, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			r.unitState = scenario.state
			resets := 0
			r.fail = func(c Command) error {
				if c.Name == "systemctl" && slices.Contains(c.Args, "reset-failed") {
					resets++
					if !slices.Contains(c.Args, unit) {
						t.Fatal("reset was not scoped to the app unit")
					}
					if scenario.resetFail {
						return testExit(1)
					}
				}
				return nil
			}
			err := h.Purge(a.Name, true)
			if scenario.state == "inactive" {
				if err != nil || resets != 0 {
					t.Fatalf("healthy unloaded unit blocked purge: resets=%d, err=%v", resets, err)
				}
			} else if resets != 1 || (err != nil) != scenario.resetFail {
				t.Fatalf("failed unit reset: resets=%d, err=%v", resets, err)
			}
			if scenario.state == "failed" && scenario.resetFail {
				if _, err := os.Stat(a.Directory); err != nil {
					t.Fatal("failed reset deleted project data")
				}
				if _, exists := r.users[a.User]; !exists {
					t.Fatal("failed reset deleted the app account")
				}
			}
		})
	}
}

func TestPurgeDeletesOnlySelectedAppAndRecordedDatabase(t *testing.T) {
	h, r, out, a := fixture(t)
	a.Database.Enabled = true
	a.Nightwatch.Enabled = true // Exercise release of a real port reservation.
	registry, err := h.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := services.Render(a, registry, h.TemplatesDir, h.Manager.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m := manifest{Version: 1, App: a.Name, User: a.User, Units: plan.Units, FPM: plan.FPM, Enabled: true}
	for _, file := range plan.Files {
		if err := h.write(file.Path, file.Data, file.Mode); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, file.Path)
	}
	if err := h.saveManifest(m); err != nil {
		t.Fatal(err)
	}
	other := config.App{Name: "other", User: "abr-other", Directory: filepath.Join(h.AppsDir, "other"), Type: "nuxt", Domain: "other.localhost"}
	if _, err := h.Manager.Register(other, nil); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(other.Directory, "uploads/keep.txt")
	if err := h.write(sentinel, []byte("other app data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other.Directory, filepath.Join(a.Directory, "outside-link")); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(h.Manager.StateDir, "deployments", a.Name, "fixture.log")
	if err := h.write(log, []byte("deployment log"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.ComposerAuth("nova.laravel.com", "fixture", "shared-token"); err != nil {
		t.Fatal(err)
	}
	r.groups = map[string]string{a.User: a.User + ":x:991:"}
	// Give getent passwd a surviving account unrelated to this app's group.
	r.users["other"] = "other:x:992:992:Other:/home/other:/bin/bash"
	r.calls = nil
	if err := h.Purge(a.Name, true); err != nil {
		t.Fatal(err)
	}
	paths := append([]string{a.Directory, "/var/lib/abr-users/" + a.User, filepath.Dir(log), h.credentialsPath(a.Name), h.credentialsEnvPath(a.Name), h.manifestPath(a.Name), h.userPath(a)}, m.Files...)
	for _, path := range paths {
		if _, err := os.Lstat(h.path(path)); !os.IsNotExist(err) {
			t.Fatalf("app resource remains: %s: %v", path, err)
		}
	}
	for _, path := range []string{sentinel, filepath.Join(h.composerDir(), "auth.json")} {
		if _, err := os.Stat(h.path(path)); err != nil {
			t.Fatalf("other app/shared data deleted: %s: %v", path, err)
		}
	}
	if _, exists := r.users[a.User]; exists || r.database || len(r.groups) != 0 {
		t.Fatal("managed account/group/database remains")
	}
	c, registry, err := h.Manager.Snapshot()
	if err != nil || len(c.Apps) != 1 || c.Apps[0].Name != other.Name {
		t.Fatalf("wrong apps after purge: %+v %v", c.Apps, err)
	}
	for _, assignment := range registry.Assignments {
		if assignment.App == a.Name {
			t.Fatal("purged app reservation remains")
		}
	}
	drop, userdel, stop := -1, -1, -1
	forgotAccess := false
	for i, cmd := range r.calls {
		if cmd.Name == "setfacl" && slices.Contains(cmd.Args, "-x") && slices.Contains(cmd.Args, filepath.Join(h.composerDir(), "auth.json")) {
			forgotAccess = true
		}
		if cmd.Name == "systemctl" && slices.Contains(cmd.Args, "stop") {
			stop = i
		}
		if cmd.Name == "userdel" {
			userdel = i
		}
		if cmd.Name == "mysql" {
			drop = i
			if !cmd.Private || string(cmd.Input) != "DROP DATABASE IF EXISTS `app`;\nDROP USER IF EXISTS 'abr-app'@'localhost';\n" || strings.Contains(strings.Join(cmd.Args, " "), "DROP") {
				t.Fatal("unscoped or exposed deletion SQL")
			}
		}
	}
	if stop < 0 || userdel <= stop || drop <= userdel || !forgotAccess || strings.Contains(out.String(), "DROP DATABASE") || strings.Contains(out.String(), "shared-token") {
		t.Fatal("data deletion ran before stopping the app, or leaked private input")
	}
}

func TestPurgeAccessCleanupPreservesReusedUID(t *testing.T) {
	h, r, _, _ := fixture(t)
	if err := h.ComposerAuth("nova.laravel.com", "fixture", "shared-token"); err != nil {
		t.Fatal(err)
	}
	r.users["another"] = "another:x:991:992:Another app:/home/another:/usr/sbin/nologin"
	r.calls = nil
	if err := h.forgetAppAccess(userRecord{UID: "991"}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range r.calls {
		if cmd.Name == "setfacl" {
			t.Fatal("retry removed another app's credential access after UID reuse")
		}
	}
}

func TestPurgeRefusesSymlinkParentsWithDotDot(t *testing.T) {
	h, _, _, a := fixture(t)
	foreign := filepath.Join(h.root, "foreign")
	if err := os.MkdirAll(filepath.Join(foreign, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(foreign, "app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreign, "sub"), filepath.Join(h.AppsDir, "link")); err != nil {
		t.Fatal(err)
	}
	a.Directory = h.AppsDir + "/link/../app"
	if _, err := h.planPurge(a); err == nil {
		t.Fatal("dot-dot path escaped the project directory through a symlink")
	}
}

func TestPurgeRefusesUnconfirmedUnsafePathsAndForeignRecords(t *testing.T) {
	for _, scenario := range []string{"unconfirmed", "database", "project-symlink", "home-symlink", "shared-path", "user"} {
		t.Run(scenario, func(t *testing.T) {
			h, r, _, a := fixture(t)
			a.Database.Enabled = true
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			confirmed := true
			switch scenario {
			case "unconfirmed":
				confirmed = false
			case "database":
				data, _ := h.read(h.credentialsPath(a.Name))
				var c credentials
				if err := json.Unmarshal(data, &c); err != nil {
					t.Fatal(err)
				}
				c.Database = "other_database"
				data, _ = json.Marshal(c)
				if err := h.write(h.credentialsPath(a.Name), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "project-symlink", "home-symlink":
				path := a.Directory
				if scenario == "home-symlink" {
					path = h.path("/var/lib/abr-users/" + a.User)
				}
				if err := os.Rename(path, path+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-saved", path); err != nil {
					t.Fatal(err)
				}
			case "shared-path":
				h.Manager.StateDir = a.Directory
				if _, err := h.planPurge(a); err == nil {
					t.Fatal("accepted app directory containing shared state")
				}
				return
			case "user":
				r.users[a.User] = strings.Replace(r.users[a.User], ":991:991:", ":992:992:", 1)
			}
			r.calls = nil
			if err := h.Purge(a.Name, confirmed); err == nil {
				t.Fatal("unsafe purge succeeded")
			}
			if _, err := os.Lstat(a.Directory); err != nil || !r.database {
				t.Fatal("refused purge deleted data")
			}
			c, err := config.Load(h.Manager.ConfigPath)
			if err != nil || len(c.Apps) != 1 {
				t.Fatal("refused purge unregistered app")
			}
			for _, cmd := range r.calls {
				if cmd.Name == "mysql" || cmd.Name == "userdel" {
					t.Fatal("refused purge deleted an account/database")
				}
			}
		})
	}
}

func TestPurgeRetainsRecordsOnFailureAndCanBeRetried(t *testing.T) {
	for _, scenario := range []string{"processes", "database-failure", "occupied-port", "shared-group"} {
		t.Run(scenario, func(t *testing.T) {
			h, r, _, a := fixture(t)
			a.Database.Enabled = true
			var listener net.Listener
			imports := map[string]int{}
			if scenario == "occupied-port" {
				var err error
				listener, err = net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				a.Nightwatch.Enabled = true
				imports["nightwatch-ingest"] = listener.Addr().(*net.TCPAddr).Port
			}
			if _, err := h.Register(a, imports); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "processes":
				r.processes = true
			case "database-failure":
				r.fail = func(c Command) error {
					if c.Name == "mysql" {
						return testExit(1)
					}
					return nil
				}
			case "shared-group":
				r.groups = map[string]string{a.User: a.User + ":x:991:other"}
			}
			if err := h.Purge(a.Name, true); err == nil {
				t.Fatal("failure was ignored")
			}
			for _, path := range []string{a.Directory, h.credentialsPath(a.Name), h.userPath(a)} {
				if _, err := os.Stat(h.path(path)); err != nil {
					t.Fatalf("retry data/record lost: %s: %v", path, err)
				}
			}
			r.processes, r.fail, r.groups = false, nil, nil
			if listener != nil {
				listener.Close()
			}
			if err := h.Purge(a.Name, true); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestPurgePreviewAndIncompleteOrUnmanagedDatabases(t *testing.T) {
	for _, scenario := range []string{"preview", "incomplete", "self-managed", "missing-project", "config-only"} {
		t.Run(scenario, func(t *testing.T) {
			h, r, out, a := fixture(t)
			a.Database.Enabled = scenario != "self-managed" && scenario != "config-only"
			if scenario == "config-only" {
				if _, err := h.Manager.Register(a, nil); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(h.path("/var/lib/abr-users/" + a.User)); err != nil {
					t.Fatal(err)
				}
			} else if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "preview":
				h.DryRun = true
			case "incomplete":
				data, _ := h.read(h.credentialsPath(a.Name))
				data = []byte(strings.Replace(string(data), `"ready": true`, `"ready": false`, 1))
				if err := h.write(h.credentialsPath(a.Name), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-project":
				if err := os.RemoveAll(a.Directory); err != nil {
					t.Fatal(err)
				}
			}
			r.calls = nil
			if err := h.Purge(a.Name, scenario != "preview"); err != nil {
				t.Fatal(err)
			}
			if scenario == "preview" {
				if len(r.calls) != 0 || strings.Contains(out.String(), "Fully removed") {
					t.Fatal("preview executed commands or reported actual removal")
				}
				if _, err := os.Stat(filepath.Join(a.Directory, ".env")); err != nil {
					t.Fatal("preview deleted files")
				}
			}
			if scenario == "self-managed" || scenario == "config-only" {
				for _, cmd := range r.calls {
					if cmd.Name == "mysql" {
						t.Fatal("purge guessed ownership of an unrecorded database")
					}
				}
			}
		})
	}
}

func TestPurgeRefusesForeignServiceConfig(t *testing.T) {
	h, _, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := "/etc/systemd/system/abr-app-queue@.service"
	if err := h.write(path, []byte("unmanaged service"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Files: []string{path}, Units: []string{"abr-app-queue@1.service"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Purge(a.Name, true); err == nil {
		t.Fatal("purge deleted unmanaged service config")
	}
	data, err := os.ReadFile(h.path(path))
	if err != nil || strings.HasPrefix(string(data), services.Marker) {
		t.Fatal("unmanaged file changed")
	}
}
