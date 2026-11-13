package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// deployUntilOptimize deploys and stops at artisan optimize, after both
// dependency installs, and returns the runuser command lines it ran.
func deployUntilOptimize(t *testing.T, h Host, r *fakeRunner, fail func(args string) bool) ([]string, error) {
	t.Helper()
	r.calls = nil
	r.fail = func(c Command) error {
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, "artisan optimize ") || (fail != nil && fail(args)) {
			return testExit(9)
		}
		return nil
	}
	err := h.Deploy([]string{"app"}, DeployOptions{NoPull: true})
	var commands []string
	for _, c := range r.calls {
		if c.Name == "runuser" {
			commands = append(commands, strings.Join(c.Args, " "))
		}
	}
	return commands, err
}

func ran(commands []string, part string) bool {
	return slices.ContainsFunc(commands, func(c string) bool { return strings.Contains(c, part) })
}

func TestDeploymentSkipsUnchangedPackageInstalls(t *testing.T) {
	h, r, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) {
		t.Helper()
		path := filepath.Join(a.Directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"scripts":{"build":"vite build","postinstall":"prisma generate"}}`)
	// The package managers write these once their trees are complete.
	write("node_modules/.package-lock.json", "{}")
	write("vendor/composer/installed.json", "{}")

	commands, _ := deployUntilOptimize(t, h, r, nil)
	if !ran(commands, "npm ci --include=dev --no-audit --no-fund --prefer-offline") || !ran(commands, "npm ci --ignore-scripts") || !ran(commands, "--download-only") {
		t.Fatalf("first deployment did not install and check packages: %q", commands)
	}
	if ran(commands, "npm run postinstall") {
		t.Fatal("npm ci already runs the package's install scripts")
	}

	commands, _ = deployUntilOptimize(t, h, r, nil)
	if ran(commands, "npm ci") || ran(commands, "--download-only") {
		t.Fatalf("unchanged packages installed or checked again: %q", commands)
	}
	if !ran(commands, "npm run postinstall") || !ran(commands, "composer install --no-dev --optimize-autoloader") || !ran(commands, "npm run build") {
		t.Fatalf("skipped install left out scripts, Composer or the build: %q", commands)
	}
	if !strings.Contains(out.String(), "skipped npm ci") {
		t.Fatal("skip not reported")
	}

	// A changed lock installs again; a failed install is not trusted later.
	write("package-lock.json", `{"lockfileVersion":3}`)
	commands, err := deployUntilOptimize(t, h, r, func(args string) bool {
		return strings.Contains(args, "npm ci --include=dev")
	})
	if err == nil || !ran(commands, "npm ci --ignore-scripts") {
		t.Fatalf("changed lock not checked and installed: %v %q", err, commands)
	}
	write("package-lock.json", "{}\n")
	if commands, _ = deployUntilOptimize(t, h, r, nil); !ran(commands, "npm ci --include=dev") {
		t.Fatal("deployment trusted packages after a failed install")
	}

	// Missing installed trees, as after a fresh clone or restore, install again.
	for _, tree := range []string{"node_modules", "vendor/composer"} {
		if err := os.RemoveAll(filepath.Join(a.Directory, tree)); err != nil {
			t.Fatal(err)
		}
	}
	if commands, _ = deployUntilOptimize(t, h, r, nil); !ran(commands, "npm ci --include=dev") || !ran(commands, "--download-only") {
		t.Fatalf("missing packages not installed: %q", commands)
	}
}

func TestDeploymentReportsStepTimes(t *testing.T) {
	h, r, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	deployUntilOptimize(t, h, r, nil)
	// The terminal shows the steps that matter; commands stay in the log.
	terminal := out.String()
	for _, line := range []string{"Deploying app", "✓ Checked before stopping", "✓ Installed Node packages", "✓ Built frontend", "✓ Ran migrations", "✗ artisan optimize failed"} {
		if !strings.Contains(terminal, line) {
			t.Fatalf("%q missing from terminal output:\n%s", line, terminal)
		}
	}
	for _, detail := range []string{"Run npm", "chown", "artisan config:clear", "Step times"} {
		if strings.Contains(terminal, detail) {
			t.Fatalf("detail %q shown in the terminal:\n%s", detail, terminal)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, "deployments/app/*.json"))
	if len(paths) != 1 {
		t.Fatalf("missing history: %v", paths)
	}
	log, _ := os.ReadFile(strings.TrimSuffix(paths[0], ".json") + ".log")
	for _, step := range []string{"Run npm ci", "Step times · app", "artisan config:clear", "total"} {
		if !strings.Contains(string(log), step) {
			t.Fatalf("%q missing from the log:\n%s", step, log)
		}
	}
	if data, _ := os.ReadFile(paths[0]); !strings.Contains(string(data), `"Step": "artisan migrate"`) {
		t.Fatalf("history lacks step times: %s", data)
	}
}

func TestStepName(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"npm", []string{"ci", "--include=dev"}, "npm ci"},
		{"npm", []string{"run", "build"}, "npm run build"},
		{"composer", []string{"install", "--no-dev"}, "composer install"},
		{"/usr/bin/php8.5", []string{"artisan", "migrate", "--force"}, "artisan migrate"},
	} {
		if got := stepName(c.name, c.args); got != c.want {
			t.Errorf("stepName(%s %v) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestRootInstallScripts(t *testing.T) {
	scripts, err := rootInstallScripts([]byte(`{"scripts":{"preinstall":"a","postinstall":"b","prepare":"c","preprepare":"d","build":"e"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// npm run starts prepare's own pre and post hooks.
	want := [][]string{{"run", "preinstall"}, {"run", "postinstall"}, {"run", "prepare"}}
	if !slices.EqualFunc(scripts, want, slices.Equal) {
		t.Fatalf("got %q", scripts)
	}
}
