package host

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Simulate package unpacking; never install packages or run host commands.
type nodeToolsRunner struct {
	*fakeRunner
	unsafe bool
}

func (r nodeToolsRunner) Run(c Command) ([]byte, error) {
	data, err := r.fakeRunner.Run(c)
	if err != nil {
		return data, err
	}
	if c.Name == "/usr/bin/node" {
		return []byte("24.1.0\n"), nil
	}
	index := slices.Index(c.Args, "--prefix")
	if c.Name != "runuser" || index < 0 {
		return data, nil
	}
	stage := c.Args[index+1]
	if err := os.MkdirAll(filepath.Join(stage, "lib"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage, "bin"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage, ".home/.npm"), 0700); err != nil {
		return nil, err
	}
	for _, tool := range []string{"npm", "npx", "ncu", "npm-check-updates", "svgo"} {
		path := filepath.Join(stage, "lib", tool+".js")
		if err := os.WriteFile(path, []byte("#!/usr/bin/env node\n"), 0777); err != nil {
			return nil, err
		}
		target := "../lib/" + tool + ".js"
		if r.unsafe && tool == "npm" {
			target = "/etc/passwd"
		}
		if err := os.Symlink(target, filepath.Join(stage, "bin", tool)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func TestNodeToolsInstallUnprivilegedAndRetireOnlyRecordedTree(t *testing.T) {
	h, runner, _, _ := fixture(t)
	runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
	h.Runner = nodeToolsRunner{fakeRunner: runner}
	if err := h.installNPM(true); err != nil {
		t.Fatal(err)
	}
	current := h.path("/opt/abr/node-tools/current")
	old, err := os.Readlink(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(old, ".home")); !os.IsNotExist(err) {
		t.Fatal("installer cache retained")
	}
	unrelated := h.path("/opt/abr/node-tools/unrelated")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.installNPM(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("recorded previous install was not retired", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("removed an unrecorded resource", err)
	}
	for _, tool := range []string{"npm", "npx", "ncu", "npm-check-updates", "svgo"} {
		info, err := os.Stat(h.path("/usr/local/bin/" + tool))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("unsafe published %s: %v %v", tool, info, err)
		}
	}
	installs := 0
	for _, c := range runner.calls {
		if slices.Contains(c.Args, "--prefix") {
			installs++
			stage := c.Args[slices.Index(c.Args, "--prefix")+1]
			for _, setting := range []string{"NPM_CONFIG_USERCONFIG=" + filepath.Join(stage, ".home/.npmrc"), "NPM_CONFIG_GLOBALCONFIG=" + filepath.Join(stage, ".npmrc-global")} {
				if !slices.Contains(c.Args, setting) {
					t.Fatalf("npm configuration is not isolated: %+v", c)
				}
			}
			if c.Name != "runuser" || c.Args[1] != "_apt" || !slices.Contains(c.Args, "--no-new-privs") || !slices.Contains(c.Args, "--ignore-scripts") || c.Dir != "/" {
				t.Fatalf("privileged package installer: %+v", c)
			}
		}
	}
	if installs != 2 {
		t.Fatalf("expected one install per setup, got %d", installs)
	}
}

func TestNodeToolsFailurePreservesExistingToolsAndCleansStage(t *testing.T) {
	for _, failure := range []string{"unpack", "unsafe-symlink", "non-symlink-bin"} {
		t.Run(failure, func(t *testing.T) {
			h, runner, _, _ := fixture(t)
			runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
			h.Runner = nodeToolsRunner{fakeRunner: runner}
			if err := h.installNPM(true); err != nil {
				t.Fatal(err)
			}
			current := h.path("/opt/abr/node-tools/current")
			previous, _ := os.Readlink(current)
			switch failure {
			case "unpack":
				runner.fail = func(c Command) error {
					if slices.Contains(c.Args, "--prefix") {
						return testExit(1)
					}
					return nil
				}
			case "unsafe-symlink":
				h.Runner = nodeToolsRunner{fakeRunner: runner, unsafe: true}
			case "non-symlink-bin":
				path := h.path("/usr/local/bin/npm")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("unmanaged"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.installNPM(true); err == nil {
				t.Fatal("failed installation reported success")
			}
			if target, _ := os.Readlink(current); target != previous {
				t.Fatal("failed installation replaced working tools")
			}
			stages, _ := filepath.Glob(h.path("/opt/abr/node-tools/release-*"))
			if len(stages) != 1 || stages[0] != previous {
				t.Fatal("failed staging tree retained", stages)
			}
		})
	}
}

func TestNodeToolsRejectsPrivilegedInstallerAccount(t *testing.T) {
	for _, ids := range []string{"0:65534", "00:65534", "42:0", "bad:65534"} {
		h, runner, _, _ := fixture(t)
		runner.users["_apt"] = fmt.Sprintf("_apt:x:%s::/nonexistent:/bin/false", ids)
		if err := h.installNodeTools(true); err == nil {
			t.Fatal("accepted privileged installer", ids)
		}
		for _, c := range runner.calls {
			if c.Name != "getent" {
				t.Fatal("modified files before validating installer", strings.Join(c.Args, " "))
			}
		}
	}
}
