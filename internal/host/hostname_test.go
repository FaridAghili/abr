package host

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestHostnameConfigurationPreservesOtherHostsAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			h, r, _, _ := fixture(t)
			old := []byte("127.0.0.1 localhost\n127.0.1.1 old-vps # primary\n127.0.1.1 duplicate\n::1 localhost ip6-localhost\n10.0.0.2 peer\n")
			if err := h.write("/etc/hosts", old, 0644); err != nil {
				t.Fatal(err)
			}
			r.fail = func(c Command) error {
				if c.Name == "hostnamectl" {
					if strings.Join(c.Args, " ") != "hostname new-vps" {
						t.Fatal("unexpected hostname command")
					}
					if fail {
						return testExit(1)
					}
					return h.write("/etc/hostname", []byte("new-vps\n"), 0644)
				}
				return nil
			}
			if err := h.configureHostname("new-vps"); (err != nil) != fail {
				t.Fatalf("hostname result: %v", err)
			}
			hosts, err := h.read("/etc/hosts")
			if err != nil {
				t.Fatal(err)
			}
			cloud := "/etc/cloud/cloud.cfg.d/99-abr-hostname.cfg"
			if fail {
				if !bytes.Equal(hosts, old) {
					t.Fatal("failed hostname command changed hosts")
				}
				if _, err := h.read(cloud); !os.IsNotExist(err) {
					t.Fatal("failed command left cloud configuration")
				}
				return
			}
			for _, text := range []string{"127.0.0.1 localhost", "::1 localhost ip6-localhost", "10.0.0.2 peer", "# primary", "127.0.1.1\tnew-vps"} {
				if !strings.Contains(string(hosts), text) {
					t.Fatalf("lost hosts entry: %s", text)
				}
			}
			if strings.Count(string(hosts), "127.0.1.1") != 1 {
				t.Fatal("duplicated hostname mapping")
			}
			if err := h.configureHostname("new-vps"); err != nil {
				t.Fatal(err)
			}
			again, _ := h.read("/etc/hosts")
			if !bytes.Equal(hosts, again) {
				t.Fatal("repeated setup changed hosts")
			}
			data, _ := h.read(cloud)
			if !strings.Contains(string(data), "preserve_hostname: true") || !strings.Contains(string(data), "manage_etc_hosts: false") {
				t.Fatal("cloud-init can reset hostname or mapping")
			}
		})
	}
}

func TestSetupRejectsInvalidHostnameBeforeAnyCommands(t *testing.T) {
	h, r, _, _ := fixture(t)
	for _, name := range []string{"", "Upper", "-vps", "vps-", "localhost", "vps.example.com", "vps\ninjected", strings.Repeat("a", 64)} {
		if err := h.Setup(SetupOptions{Hostname: name, RoadRunnerVersion: DefaultRoadRunnerVersion}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if len(r.calls) != 0 {
		t.Fatal("invalid hostname changed host")
	}
}
