package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLaravelAccessUsesRecordedIdentityAndLiteralArguments(t *testing.T) {
	h, runner, out, app := fixture(t)
	if _, err := h.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	args := []string{"custom:admin", "--name=Jane Doe", "$(touch /tmp/unsafe)", "--force"}
	process, err := h.LaravelProcess(app.Name, args, false)
	if err != nil {
		t.Fatal(err)
	}
	if process.Path != "/usr/sbin/runuser" || process.Dir != app.Directory || !slices.Equal(process.Args[len(process.Args)-len(args):], args) {
		t.Fatalf("unsafe invocation: %+v", process)
	}
	for _, want := range []string{app.User, "HOME=/var/lib/abr-users/" + app.User, "APP_ENV=production", "APP_DEBUG=false", "--no-new-privs", "/usr/bin/php8.5", "artisan"} {
		if !slices.Contains(process.Args, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(out.String(), "Jane Doe") {
		t.Fatal("command arguments logged")
	}
	shell, err := h.LaravelProcess(app.Name, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(shell.Args, "HISTFILE=/dev/null") || !strings.Contains(shell.Args[len(shell.Args)-1], "--noprofile --norc -i") {
		t.Fatal("unsafe shell startup")
	}
	runner.users[app.User] = strings.Replace(runner.users[app.User], ":991:991:", ":0:0:", 1)
	if _, err := h.LaravelProcess(app.Name, nil, false); err == nil {
		t.Fatal("accepted root account")
	}
}

func TestLaravelAccessRejectsUnregisteredAccountAndMissingDependencies(t *testing.T) {
	h, _, _, app := fixture(t)
	if _, err := h.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(h.userPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.userPath(app)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.LaravelProcess(app.Name, nil, false); err == nil {
		t.Fatal("accepted unrecorded account")
	}
	if err := os.WriteFile(h.userPath(app), record, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(app.Directory, "vendor/autoload.php")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.LaravelProcess(app.Name, nil, false); err == nil {
		t.Fatal("accepted undeployed app")
	}
	h.DryRun = true
	if process, err := h.LaravelProcess(app.Name, []string{"cache:clear"}, false); err != nil || process != nil {
		t.Fatal("preview prepared executable", err)
	}
}
