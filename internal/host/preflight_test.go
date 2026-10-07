package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"abr/internal/services"
)

func TestNuxtRegistrationAndPreflightNeedOnlyFrontendDependencies(t *testing.T) {
	h, runner, _, a := fixture(t)
	a.Type, a.Web.Driver = "nuxt", ""
	for _, name := range []string{".env", "artisan", "composer.json", "composer.lock"} {
		if err := os.Remove(filepath.Join(a.Directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.preflight(a, nil, strings.Repeat("a", 40), true); err != nil {
		t.Fatal(err)
	}
	node, npm := false, false
	for _, command := range runner.calls {
		if command.Name == "mysql" || strings.Contains(command.Name, "php") || slices.Contains(command.Args, "composer") || slices.Contains(command.Args, "artisan") {
			t.Fatalf("Nuxt ran Laravel host work: %s %v", command.Name, command.Args)
		}
		node = node || slices.Contains(command.Args, "node")
		npm = npm || slices.Contains(command.Args, "npm")
	}
	if !node || !npm {
		t.Fatal("Nuxt did not check frontend dependencies")
	}
	if _, err := os.Stat(h.path(h.credentialsPath(a.Name))); !os.IsNotExist(err) {
		t.Fatal("Nuxt registration created database credentials")
	}
}

func TestPreflightFailuresLeaveRunningAppAndCheckoutUntouched(t *testing.T) {
	for _, failure := range []string{"env", "composer-lock", "package-lock", "platform", "composer-auth", "npm", "fetch", "diverged", "incoming-lock", "incoming-platform"} {
		t.Run(failure, func(t *testing.T) {
			h, runner, out, a := fixture(t)
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			path := "/etc/caddy/abr.d/abr-app.caddy"
			contents := []byte(services.Marker + "\nlive app\n")
			if err := h.write(path, contents, 0644); err != nil {
				t.Fatal(err)
			}
			if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Files: []string{path}, Units: []string{"abr-app-queue@1.service"}, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			noPull := true
			switch failure {
			case "env":
				os.Remove(filepath.Join(a.Directory, ".env"))
			case "composer-lock":
				os.Remove(filepath.Join(a.Directory, "composer.lock"))
			case "package-lock":
				os.Remove(filepath.Join(a.Directory, "package-lock.json"))
			case "fetch", "diverged", "incoming-lock", "incoming-platform":
				noPull = false
			}
			runner.calls = nil
			runner.fail = func(cmd Command) error {
				args := strings.Join(cmd.Args, " ")
				if cmd.Name != "runuser" {
					return nil
				}
				if (failure == "platform" || failure == "incoming-platform") && strings.Contains(args, "check-platform-reqs --lock") {
					return testExit(2)
				}
				if failure == "composer-auth" && slices.Contains(cmd.Args, "--download-only") {
					return testExit(100)
				}
				if failure == "npm" && strings.Contains(args, "npm ci --dry-run") {
					return testExit(1)
				}
				if failure == "fetch" && slices.Contains(cmd.Args, "fetch") {
					return testExit(128)
				}
				if failure == "diverged" && slices.Contains(cmd.Args, "merge-base") {
					return testExit(1)
				}
				if failure == "incoming-lock" && slices.Contains(cmd.Args, "cat-file") && strings.HasSuffix(cmd.Args[len(cmd.Args)-1], ":composer.lock") {
					return testExit(128)
				}
				return nil
			}
			err := h.Deploy([]string{a.Name}, DeployOptions{NoPull: noPull})
			if err == nil {
				t.Fatal("failed preflight reported success")
			}
			if failure == "composer-auth" && !strings.Contains(err.Error(), "abr composer auth") {
				t.Fatal("authentication failure lost credential guidance")
			}
			for _, cmd := range runner.calls {
				if cmd.Name == "systemctl" || cmd.Name == "caddy" || (cmd.Name == "runuser" && slices.Contains(cmd.Args, "merge")) {
					t.Fatalf("preflight modified running app: %s %v", cmd.Name, cmd.Args)
				}
			}
			got, err := h.read(path)
			if err != nil || !bytes.Equal(contents, got) {
				t.Fatal("live routing changed")
			}
			manifest, _, err := h.loadManifest(a)
			if err != nil || !manifest.Enabled {
				t.Fatal("app disabled")
			}
			stages, _ := filepath.Glob(filepath.Join(h.AppsDir, ".abr-preflight-*"))
			if len(stages) != 0 {
				t.Fatal("preflight directory leaked")
			}
			if strings.Contains(out.String(), "Deployed app") {
				t.Fatal("false success")
			}
		})
	}
}

func TestPreflightChecksFetchedFilesAndMergesOnlyCheckedCommit(t *testing.T) {
	h, runner, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	merged := false
	h.Runner = transferRunner{func(cmd Command) ([]byte, error) {
		if cmd.Name == "runuser" && slices.Contains(cmd.Args, "rev-parse") {
			return []byte(sha), nil
		}
		if cmd.Name == "runuser" && slices.Contains(cmd.Args, "show") && strings.HasSuffix(cmd.Args[len(cmd.Args)-1], ":package.json") {
			return []byte(`{"name":"incoming"}`), nil
		}
		if cmd.Name == "runuser" && slices.Contains(cmd.Args, "npm") && slices.Contains(cmd.Args, "--dry-run") {
			data, err := os.ReadFile(filepath.Join(cmd.Dir, "package.json"))
			if err != nil || !bytes.Contains(data, []byte("incoming")) || cmd.Dir == a.Directory || !cmd.Private || !slices.Contains(cmd.Args, "--ignore-scripts") {
				t.Fatal("preflight did not isolate fetched dependencies")
			}
		}
		if cmd.Name == "runuser" && slices.Contains(cmd.Args, "merge") {
			merged = true
			if cmd.Args[len(cmd.Args)-1] != sha {
				t.Fatal("merged different commit")
			}
			return nil, fmt.Errorf("stop after checked merge")
		}
		return runner.Run(cmd)
	}}
	if err := h.Deploy([]string{a.Name}, DeployOptions{}); err == nil || !merged {
		t.Fatalf("did not reach checked merge: %v", err)
	}
}
