package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServerUpdateOrderAndFailureReporting(t *testing.T) {
	for _, failure := range []string{"", "update", "full-upgrade", "autoremove", "autoclean", "self-update", "--jsonUpgraded", "omz", "autosuggestions", "highlighting"} {
		t.Run("failure="+failure, func(t *testing.T) {
			h, runner, out, _ := fixture(t)
			runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
			h.Runner = shellRunner{nodeToolsRunner{fakeRunner: runner}}
			prepareShellFixture(t, h)
			if err := h.installNPM(true); err != nil {
				t.Fatal(err)
			}
			if err := h.write("/usr/local/bin/composer", []byte("fixture"), 0755); err != nil {
				t.Fatal(err)
			}
			runner.calls, runner.fail = nil, func(c Command) error {
				shellFailure := failure == "omz" && c.Name == "zsh" || failure == "autosuggestions" && c.Name == "git" && slices.Contains(c.Args, "pull") && strings.HasSuffix(c.Args[1], "zsh-autosuggestions") || failure == "highlighting" && c.Name == "git" && slices.Contains(c.Args, "pull") && strings.HasSuffix(c.Args[1], "zsh-syntax-highlighting")
				if failure != "" && (slices.Contains(c.Args, failure) || shellFailure) {
					return testExit(1)
				}
				return nil
			}
			out.Reset()
			err := h.Update()
			if (err != nil) != (failure != "") {
				t.Fatalf("update failure=%q: %v", failure, err)
			}
			if got := strings.Contains(out.String(), "Server update complete"); got != (failure == "") {
				t.Fatal("server update reported incorrect completion")
			}
			var operations []string
			for _, c := range runner.calls {
				if c.Name == "zsh" {
					operations = append(operations, "omz")
				}
				if c.Name == "git" && slices.Contains(c.Args, "pull") {
					if !slices.Contains(c.Args, "--ff-only") {
						t.Fatal("plugin update can overwrite local changes")
					}
					if strings.HasSuffix(c.Args[1], "zsh-autosuggestions") {
						operations = append(operations, "autosuggestions")
					} else {
						operations = append(operations, "highlighting")
					}
				}
				for _, name := range []string{"update", "full-upgrade", "autoremove", "autoclean", "self-update", "--jsonUpgraded"} {
					if slices.Contains(c.Args, name) {
						operations = append(operations, name)
					}
				}
				if c.Name == "apt-get" && (!c.Stream || !slices.Contains(c.Env, "DEBIAN_FRONTEND=noninteractive") || !slices.Contains(c.Args, "DPkg::Lock::Timeout=120")) {
					t.Fatal("apt update is not scriptable or does not wait for package locks")
				}
				if c.Name == "/usr/local/bin/composer" && (c.Dir != "/" || !slices.Contains(c.Args, "--no-plugins") || !slices.Contains(c.Args, "--no-scripts") || !slices.Contains(c.Env, "COMPOSER_HOME="+filepath.Join(h.Manager.StateDir, "composer-update"))) {
					t.Fatal("Composer self-update reused app configuration or scripts")
				}
			}
			want := []string{"update", "full-upgrade", "autoremove", "autoclean", "self-update", "--jsonUpgraded", "omz", "autosuggestions", "highlighting"}
			if failure != "" {
				want = want[:slices.Index(want, failure)+1]
			}
			if !slices.Equal(operations, want) {
				t.Fatalf("update steps = %v; want %v", operations, want)
			}
		})
	}
}

func TestServerUpdatePreviewDoesNotRunCommandsOrCreateFiles(t *testing.T) {
	h, runner, out, _ := fixture(t)
	h.DryRun = true
	if err := h.Update(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || strings.Contains(out.String(), "Server update complete") || !strings.Contains(out.String(), "ncu -g") {
		t.Fatal("preview executed commands or reported real updates")
	}
	for _, component := range []string{"Oh My Zsh", "zsh-autosuggestions", "zsh-syntax-highlighting"} {
		if !strings.Contains(out.String(), component) {
			t.Fatalf("preview omitted shell update: %s", component)
		}
	}
	if _, err := os.Stat(filepath.Join(h.Manager.StateDir, "composer-update")); !os.IsNotExist(err) {
		t.Fatal("preview created Composer state")
	}
}

func TestServerUpdateRequiresShellBeforeChangingPackages(t *testing.T) {
	h, runner, out, _ := fixture(t)
	if err := h.Update(); err == nil || !strings.Contains(err.Error(), "run abr setup") {
		t.Fatalf("missing shell did not require setup: %v", err)
	}
	if len(runner.calls) != 0 || strings.Contains(out.String(), "Server update complete") {
		t.Fatal("changed host or reported completion with missing shell")
	}
}
