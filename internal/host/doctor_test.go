package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"abr/internal/ports"
)

type doctorRunner struct {
	calls             []Command
	states, listeners string
	fail              string
}

func (r *doctorRunner) Run(c Command) ([]byte, error) {
	r.calls = append(r.calls, c)
	if c.Name == r.fail {
		return nil, fmt.Errorf("inspection failed")
	}
	switch c.Name {
	case "systemctl":
		return []byte(r.states), nil
	case "ss":
		return []byte(r.listeners), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", c.Name)
	}
}

func TestDoctorListenerOwnershipAndMissingServices(t *testing.T) {
	const unit = "abr-app-nuxt.service"
	const group = "/system.slice/" + unit
	for _, tc := range []struct {
		name, listener, group, active, load, want, fail                                        string
		disabled, uninstalled, busy, noGroupFile, omittedState, missingUnit, emptyServiceGroup bool
		ok                                                                                     bool
	}{
		{name: "expected child process", listener: "owned", group: group + "/worker", want: "owned by active " + unit, ok: true},
		{name: "expected parent", listener: "owned", group: group, ok: true},
		{name: "unrelated service", listener: "owned", group: "/system.slice/foreign.service", want: "port conflict"},
		{name: "other app same user", listener: "owned", group: "/system.slice/abr-other-nuxt.service", want: "port conflict"},
		{name: "prefix is not a child", listener: "owned", group: group + "-other", want: "port conflict"},
		{name: "hidden owner", listener: "hidden", want: "cannot verify listener owner"},
		{name: "vanished process", listener: "owned", noGroupFile: true, want: "cannot verify listener PID"},
		{name: "missing cgroup", listener: "owned", want: "unified cgroup information is unavailable"},
		{name: "expected listener stopped", active: "inactive", want: "expected listener is missing"},
		{name: "active without listener", want: "expected listener is missing"},
		{name: "unloaded service", load: "not-found", want: "expected listener is missing"},
		{name: "enabled config not deployed", missingUnit: true, want: "missing its recorded endpoint service"},
		{name: "disabled free", disabled: true, want: "available (app disabled)", ok: true},
		{name: "registered free", disabled: true, uninstalled: true, want: "available (service not deployed)", ok: true},
		{name: "disabled but running", disabled: true, listener: "owned", group: group, want: "recorded as disabled"},
		{name: "unmanaged listener", disabled: true, uninstalled: true, listener: "owned", group: group, want: "port conflict"},
		{name: "snapshot race", busy: true, disabled: true, want: "cannot confirm port availability"},
		{name: "mixed IPv4 IPv6 owners", listener: "mixed", group: group, want: "port conflict"},
		{name: "multiple owners", listener: "shared", group: group, want: "port conflict"},
		{name: "failed service with listener", active: "failed", listener: "owned", group: group, want: "loaded/failed"},
		{name: "inactive service with foreign listener", active: "inactive", emptyServiceGroup: true, listener: "owned", group: "/system.slice/foreign.service", want: "port is occupied while expected service"},
		{name: "active service missing ownership metadata", emptyServiceGroup: true, listener: "owned", group: group, want: "cannot verify listener ownership"},
		{name: "ss inspection fails", fail: "ss", want: "cannot verify TCP listener ownership"},
		{name: "systemctl fails", fail: "systemctl", want: "cannot verify systemd service state"},
		{name: "service omitted", omittedState: true, want: "cannot verify services"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, out, app := fixture(t)
			app.Type, app.Web.Driver = "nuxt", ""
			r, err := h.Manager.Register(app, nil)
			if err != nil {
				t.Fatal(err)
			}
			port, _ := r.Lookup(app.Name, "nuxt-http")
			if !tc.uninstalled {
				recorded := []string{unit}
				if tc.missingUnit {
					recorded = nil
				}
				if err := h.saveManifest(manifest{Version: 1, App: app.Name, User: app.User, Enabled: !tc.disabled, Units: recorded}); err != nil {
					t.Fatal(err)
				}
			}
			stateGroup := group
			if tc.emptyServiceGroup {
				stateGroup = ""
			}
			active, load := tc.active, tc.load
			if active == "" {
				active = "active"
			}
			if load == "" {
				load = "loaded"
			}
			runner := &doctorRunner{fail: tc.fail, states: fmt.Sprintf("Id=%s\nLoadState=%s\nActiveState=%s\nControlGroup=%s\n", unit, load, active, stateGroup)}
			if tc.omittedState {
				runner.states = ""
			}
			row := fmt.Sprintf("LISTEN 0 511 127.0.0.1:%d 0.0.0.0:*", port)
			switch tc.listener {
			case "owned":
				runner.listeners = row + ` users:(("node",pid=123,fd=18))`
			case "hidden":
				runner.listeners = row
			case "mixed":
				runner.listeners = row + fmt.Sprintf(" users:((\"node\",pid=123,fd=18))\nLISTEN 0 511 [::1]:%d [::]:* users:((\"other\",pid=124,fd=4))", port)
			case "shared":
				runner.listeners = row + ` users:(("node",pid=123,fd=18),("other",pid=124,fd=4))`
			}
			if !tc.noGroupFile {
				if err := h.write("/proc/123/cgroup", []byte("0::"+tc.group+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.write("/proc/124/cgroup", []byte("0::/system.slice/foreign.service\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.busy {
				h.Manager.Probe = func(int) error { return ports.ErrOccupied }
			}
			h.Runner = runner
			out.Reset()
			err = h.Doctor()
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected success/error: %v\n%s", err, out)
			}
			combined := out.String() + fmt.Sprint(err)
			if tc.want != "" && !strings.Contains(combined, tc.want) {
				t.Fatalf("missing %q: %s", tc.want, combined)
			}
			if strings.Contains(tc.want, "cannot verify listener") || tc.busy || tc.want == "unified cgroup information is unavailable" {
				if !strings.Contains(out.String(), "UNVERIFIED:") {
					t.Fatal("inconclusive check was presented as a definite error")
				}
			}
			if !tc.ok && strings.Contains(out.String(), "all 1 app endpoint checks passed") {
				t.Fatal("false success")
			}
			for _, call := range runner.calls {
				if !call.Private || (call.Name != "ss" && (call.Name != "systemctl" || call.Args[0] != "show")) {
					t.Fatal("doctor leaked metadata or mutated host")
				}
			}
		})
	}
}

func TestDoctorPortableChecksDoNotClaimListenerConflicts(t *testing.T) {
	h, _, out, app := fixture(t)
	app.Type, app.Web.Driver = "nuxt", ""
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	c, r, err := h.Manager.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	h.Manager.Probe = func(int) error { return ports.ErrOccupied }
	if err := h.portableDoctor(c, r); err == nil || !strings.Contains(out.String(), "UNVERIFIED") || strings.Contains(out.String(), "port conflict") {
		t.Fatalf("portable check claims unverified health/conflict: %v %s", err, out)
	}
}

func TestDoctorBatchesEndpointsAndRecognizesOctaneChildren(t *testing.T) {
	h, _, out, app := fixture(t)
	app.Web.Driver, app.InertiaSSR.Enabled, app.Nightwatch.Enabled = "octane", true, true
	r, err := h.Manager.Register(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	units := []string{"abr-app-octane.service", "abr-app-inertia-ssr.service", "abr-app-nightwatch.service"}
	if err := h.saveManifest(manifest{Version: 1, App: app.Name, User: app.User, Enabled: true, Units: units}); err != nil {
		t.Fatal(err)
	}
	runner := &doctorRunner{}
	for _, unit := range units {
		runner.states += fmt.Sprintf("Id=%s\nLoadState=loaded\nActiveState=active\nControlGroup=/system.slice/%s\n\n", unit, unit)
	}
	for i, purpose := range app.Endpoints() {
		port, _ := r.Lookup(app.Name, purpose)
		pid := fmt.Sprint(200 + i)
		if err := h.write(filepath.Join("/proc", pid, "cgroup"), []byte("0::/system.slice/"+endpointUnit(app.Name, purpose)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		runner.listeners += fmt.Sprintf("LISTEN 0 4096 127.0.0.1:%d 0.0.0.0:* users:((\"rr\",pid=%s,fd=8))\n", port, pid)
	}
	h.Runner = runner
	if err := h.Doctor(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(runner.calls) != 2 || !strings.Contains(out.String(), "all 4 app endpoint checks passed") {
		t.Fatalf("not batched or incomplete: %s", out)
	}
	if strings.Count(strings.Join(runner.calls[0].Args, " "), "abr-app-octane.service") != 1 {
		t.Fatal("octane service queried twice")
	}
}

func TestDoctorRefusesInvalidReservationsAndManifest(t *testing.T) {
	for _, corrupt := range []string{"reservation", "manifest"} {
		t.Run(corrupt, func(t *testing.T) {
			h, _, out, app := fixture(t)
			app.Type, app.Web.Driver = "nuxt", ""
			if _, err := h.Manager.Register(app, nil); err != nil {
				t.Fatal(err)
			}
			if corrupt == "reservation" {
				if err := ports.Empty().Save(h.Manager.RegistryPath()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := h.saveManifest(manifest{Version: 1, App: app.Name, User: "wrong", Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
			h.Runner = &doctorRunner{}
			if err := h.Doctor(); err == nil || !strings.Contains(out.String(), "ERROR") {
				t.Fatalf("invalid state accepted: %v %s", err, out)
			}
		})
	}
}

func TestDoctorMetadataParsers(t *testing.T) {
	for _, data := range []string{"garbage", "LISTEN 0 1 bad *:*", "LISTEN 0 1 [::]:65536 [::]:*"} {
		if _, err := parseDoctorListeners(data); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	for _, data := range []string{"", "bad", "Id=abr-app-nuxt.service\nActiveState=active"} {
		if _, err := parseDoctorUnits(data); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestDoctorPreviewDoesNotInspectHost(t *testing.T) {
	h, _, out, app := fixture(t)
	app.Type, app.Web.Driver = "nuxt", ""
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	h.DryRun = true
	runner := &doctorRunner{}
	h.Runner = runner
	before, _ := os.ReadFile(h.Manager.RegistryPath())
	if err := h.Doctor(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(h.Manager.RegistryPath())
	if len(runner.calls) != 0 || string(before) != string(after) || !strings.Contains(out.String(), "runtime health was not checked") {
		t.Fatal("preview inspected runtime or claimed health")
	}
}
