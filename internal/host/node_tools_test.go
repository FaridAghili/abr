package host

import (
	"encoding/json"
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
	if c.Name == "runuser" && slices.Contains(c.Args, "prefix") && slices.Contains(c.Args, "--global") {
		return []byte("/usr\n"), nil
	}
	if c.Name == "runuser" && slices.Contains(c.Args, "--jsonUpgraded") {
		return []byte(`{}`), nil
	}
	if c.Name == "runuser" && slices.Contains(c.Args, "list") && slices.Contains(c.Args, "--json") {
		for _, arg := range c.Args {
			if prefix, ok := strings.CutPrefix(arg, "NPM_CONFIG_PREFIX="); ok && strings.HasPrefix(filepath.Base(prefix), "release-") {
				return os.ReadFile(filepath.Join(prefix, ".fake-packages.json"))
			}
		}
		return []byte(`{"dependencies":{"npm":{"version":"12.0.0"}}}`), nil
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
	inventory := struct {
		Dependencies map[string]struct{ Version string } `json:"dependencies"`
	}{Dependencies: map[string]struct{ Version string }{}}
	tools := []string{}
	for _, arg := range c.Args[slices.Index(c.Args, "--no-fund")+1:] {
		at := strings.LastIndex(arg, "@")
		name, version := arg[:at], arg[at+1:]
		if version == "latest" {
			version = map[string]string{"npm": "12.0.0", "npm-check-updates": "20.0.0", "svgo": "4.0.0"}[name]
		}
		inventory.Dependencies[name] = struct{ Version string }{version}
		switch name {
		case "npm":
			tools = append(tools, "npm", "npx")
		case "npm-check-updates":
			tools = append(tools, "ncu", "npm-check-updates")
		default:
			tools = append(tools, filepath.Base(name))
		}
	}
	data, _ = json.Marshal(inventory)
	if err := os.WriteFile(filepath.Join(stage, ".fake-packages.json"), data, 0644); err != nil {
		return nil, err
	}
	for _, tool := range tools {
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

func TestGlobalNodeUpgradeKeepsOptionalAndAdditionalPackages(t *testing.T) {
	h, runner, _, _ := fixture(t)
	runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
	base := nodeToolsRunner{fakeRunner: runner}
	h.Runner = base
	if err := h.installNPM(true); err != nil {
		t.Fatal(err)
	}
	previous, _ := h.nodeToolsDirectory()
	h.Runner = transferRunner{func(c Command) ([]byte, error) {
		managed := slices.Contains(c.Args, "NPM_CONFIG_PREFIX="+previous)
		if c.Name == "runuser" && slices.Contains(c.Args, "--jsonUpgraded") {
			if managed {
				return []byte(`{"npm":"12.1.0","npm-check-updates":"21.0.0","svgo":"5.0.0"}`), nil
			}
			return []byte(`{"@example/tool":"2.0.0"}`), nil
		}
		if c.Name == "runuser" && !managed && slices.Contains(c.Args, "list") {
			return []byte(`{"dependencies":{"npm":{"version":"11.0.0"},"@example/tool":{"version":"1.0.0"}}}`), nil
		}
		return base.Run(c)
	}}
	if err := h.updateNodeTools(); err != nil {
		t.Fatal(err)
	}
	current, _ := h.nodeToolsDirectory()
	if current == previous {
		t.Fatal("global upgrades did not publish a new installation")
	}
	data, err := os.ReadFile(filepath.Join(current, ".fake-packages.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"12.1.0", "21.0.0", "5.0.0", "2.0.0"} {
		if !strings.Contains(string(data), version) {
			t.Fatalf("global upgrade or package missing: %s", data)
		}
	}
	if target, _ := os.Readlink(h.path("/usr/local/bin/tool")); !strings.HasPrefix(target, current+"/") {
		t.Fatal("additional global tool was not published")
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatal("recorded previous tree was not retired")
	}
}

func TestGlobalNodeUpgradeFailureKeepsPublishedTools(t *testing.T) {
	for _, failure := range []string{"check", "invalid-json", "unknown-package", "invalid-version", "install"} {
		t.Run(failure, func(t *testing.T) {
			h, runner, out, _ := fixture(t)
			runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
			base := nodeToolsRunner{fakeRunner: runner}
			h.Runner = base
			if err := h.installNPM(false); err != nil {
				t.Fatal(err)
			}
			previous, _ := h.nodeToolsDirectory()
			h.Runner = transferRunner{func(c Command) ([]byte, error) {
				if slices.Contains(c.Args, "--jsonUpgraded") {
					switch failure {
					case "check":
						return nil, testExit(1)
					case "invalid-json":
						return []byte("failed"), nil
					case "unknown-package":
						return []byte(`{"unexpected":"1.0.0"}`), nil
					case "invalid-version":
						return []byte(`{"npm":"https://example.invalid/package"}`), nil
					default:
						return []byte(`{"npm":"12.1.0"}`), nil
					}
				}
				if failure == "install" && slices.Contains(c.Args, "--prefix") {
					return nil, testExit(1)
				}
				return base.Run(c)
			}}
			out.Reset()
			if err := h.updateNodeTools(); err == nil {
				t.Fatal("failed global upgrade reported success")
			}
			current, _ := h.nodeToolsDirectory()
			if current != previous || strings.Contains(out.String(), "up to date") {
				t.Fatal("failed global upgrade changed tools or reported success")
			}
			if paths, _ := filepath.Glob(h.path(nodeToolsBase + "/.check-*")); len(paths) != 0 {
				t.Fatal("global check cache retained")
			}
		})
	}
}

func TestNodeSetupAppliesReportedGlobalUpgrades(t *testing.T) {
	h, runner, _, _ := fixture(t)
	runner.users["_apt"] = "_apt:x:42:65534::/nonexistent:/usr/sbin/nologin"
	base := nodeToolsRunner{fakeRunner: runner}
	checked := false
	h.Runner = transferRunner{func(c Command) ([]byte, error) {
		if slices.Contains(c.Args, "--jsonUpgraded") {
			checked = true
			// The freshly installed checker executes after publication.
			if _, err := h.nodeToolsDirectory(); err != nil {
				t.Fatal("ncu ran before the installation was recorded and published")
			}
			return []byte(`{"npm":"12.1.0","svgo":"5.0.0"}`), nil
		}
		return base.Run(c)
	}}
	if err := h.installNPM(true); err != nil {
		t.Fatal(err)
	}
	current, _ := h.nodeToolsDirectory()
	data, err := os.ReadFile(filepath.Join(current, ".fake-packages.json"))
	if err != nil || !checked || !strings.Contains(string(data), "12.1.0") || !strings.Contains(string(data), "5.0.0") {
		t.Fatalf("Node setup did not apply global upgrades: %s %v", data, err)
	}
	builtin, err := os.ReadFile(filepath.Join(current, "lib/node_modules/npm/npmrc"))
	if err != nil || !strings.Contains(string(builtin), "prefix="+h.path(nodeToolsBase+"/current")) {
		t.Fatalf("ordinary global npm commands have the wrong prefix: %v", err)
	}
}
