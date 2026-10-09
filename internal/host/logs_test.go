package host

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func logFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep until cleared\n"), 0640); err != nil {
		t.Fatal(err)
	}
}

func assertLogSize(t *testing.T, path string, empty bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || (info.Size() == 0) != empty {
		t.Fatalf("unexpected contents for %s (empty=%t): %v %v", path, empty, info, err)
	}
}

func TestClearLogsSelectionAndTypes(t *testing.T) {
	for _, kind := range []string{"all", "application", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, out, a := fixture(t)
			if _, err := h.Manager.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			b := a
			b.Name, b.User, b.Domain, b.Type = "web", "abr-web", "web.localhost", "nuxt"
			b.Directory = filepath.Join(h.AppsDir, b.Name)
			b.Web.Driver = ""
			if _, err := h.Manager.Register(b, nil); err != nil {
				t.Fatal(err)
			}
			appLogs := []string{filepath.Join(a.Directory, "storage/logs/laravel.log"), filepath.Join(a.Directory, "storage/logs/daily/laravel-2026-10-09.log"), filepath.Join(a.Directory, "logs/worker.log")}
			deployment := filepath.Join(h.Manager.StateDir, "deployments", a.Name, "run.log")
			other := filepath.Join(b.Directory, "logs/server.log")
			preserved := []string{filepath.Join(a.Directory, "storage/app/public/upload.log"), filepath.Join(a.Directory, "storage/logs/.gitignore"), filepath.Join(h.Manager.StateDir, "deployments", a.Name, "run.json")}
			for _, path := range append(append(append([]string{}, appLogs...), deployment, other), preserved...) {
				logFile(t, path)
			}
			before, _ := os.Stat(appLogs[0])
			writer, err := os.OpenFile(appLogs[0], os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if err := h.ClearLogs([]string{a.Name}, ClearLogsOptions{Yes: true, Type: kind}); err != nil {
				t.Fatal(err)
			}
			for _, path := range appLogs {
				assertLogSize(t, path, kind != "deployment")
			}
			assertLogSize(t, deployment, kind != "application")
			assertLogSize(t, other, false)
			for _, path := range preserved {
				assertLogSize(t, path, false)
			}
			assertLogSize(t, filepath.Join(a.Directory, ".env"), false)
			after, _ := os.Stat(appLogs[0])
			if !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("clearing replaced the file or changed its permissions")
			}
			if _, err := writer.WriteString("new log\n"); err != nil {
				t.Fatal(err)
			}
			assertLogSize(t, appLogs[0], false)
			if err := h.ClearLogs(nil, ClearLogsOptions{All: true, Yes: true}); err != nil {
				t.Fatal(err)
			}
			assertLogSize(t, other, true)
			if len(runner.calls) != 0 || !strings.Contains(out.String(), "web: cleared 1 log files") {
				t.Fatal("clearing ran unrelated host commands or missed the other app")
			}
		})
	}
}

func TestClearLogsValidationAndPreview(t *testing.T) {
	h, _, out, a := fixture(t)
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Directory, "storage/logs/laravel.log")
	logFile(t, path)
	for _, selection := range []struct {
		names []string
		o     ClearLogsOptions
	}{
		{[]string{a.Name}, ClearLogsOptions{}},
		{nil, ClearLogsOptions{Yes: true}},
		{[]string{a.Name}, ClearLogsOptions{All: true, Yes: true}},
		{[]string{a.Name}, ClearLogsOptions{Yes: true, Type: "journal"}},
		{[]string{a.Name, "missing"}, ClearLogsOptions{Yes: true}},
		{[]string{a.Name, a.Name}, ClearLogsOptions{Yes: true}},
	} {
		if err := h.ClearLogs(selection.names, selection.o); err == nil {
			t.Fatalf("invalid selection accepted: %+v", selection)
		}
		assertLogSize(t, path, false)
	}
	h.DryRun = true
	out.Reset()
	if err := h.ClearLogs([]string{a.Name}, ClearLogsOptions{}); err != nil {
		t.Fatal(err)
	}
	assertLogSize(t, path, false)
	if !strings.Contains(out.String(), "Would clear app") || strings.Contains(out.String(), "app: cleared") {
		t.Fatal("preview claimed to clear files")
	}
	h.DryRun = false
	if err := os.RemoveAll(filepath.Join(a.Directory, "storage/logs")); err != nil {
		t.Fatal(err)
	}
	if err := h.ClearLogs([]string{a.Name}, ClearLogsOptions{Yes: true}); err != nil {
		t.Fatalf("missing log directories should be harmless: %v", err)
	}
}

func TestClearLogsRejectsUnsafeFilesAndDirectories(t *testing.T) {
	for _, attack := range []string{"symlink", "hardlink", "fifo", "directory", "parent", "nested-directory"} {
		t.Run(attack, func(t *testing.T) {
			h, _, out, a := fixture(t)
			if _, err := h.Manager.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(h.root, "outside")
			victim := filepath.Join(outside, "secret.log")
			logFile(t, victim)
			logs := filepath.Join(a.Directory, "storage/logs")
			if err := os.MkdirAll(logs, 0755); err != nil {
				t.Fatal(err)
			}
			var err error
			switch attack {
			case "symlink":
				err = os.Symlink(victim, filepath.Join(logs, "linked.log"))
			case "hardlink":
				err = os.Link(victim, filepath.Join(logs, "linked.log"))
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(logs, "pipe.log"), 0600)
			case "directory":
				if err = os.Remove(logs); err == nil {
					err = os.Symlink(outside, logs)
				}
			case "nested-directory":
				err = os.Symlink(outside, filepath.Join(logs, "external"))
			case "parent":
				if err = os.Mkdir(filepath.Join(outside, "logs"), 0755); err == nil {
					err = os.RemoveAll(filepath.Join(a.Directory, "storage"))
				}
				if err == nil {
					err = os.Symlink(outside, filepath.Join(a.Directory, "storage"))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := h.ClearLogs([]string{a.Name}, ClearLogsOptions{Yes: true, Type: "application"}); err == nil {
				t.Fatal("unsafe log target accepted")
			}
			assertLogSize(t, victim, false)
			if !strings.Contains(out.String(), "clearing incomplete") {
				t.Fatal("failure was reported as success")
			}
		})
	}
}

func TestClearAllLogsContinuesAfterAppFailure(t *testing.T) {
	h, _, out, a := fixture(t)
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Name, b.User, b.Domain = "second", "abr-second", "second.localhost"
	b.Directory = filepath.Join(h.AppsDir, b.Name)
	if _, err := h.Manager.Register(b, nil); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(a.Directory, "logs/bad.log")
	good := filepath.Join(b.Directory, "storage/logs/laravel.log")
	victim := filepath.Join(h.root, "preserve.log")
	logFile(t, victim)
	logFile(t, good)
	if err := os.MkdirAll(filepath.Dir(bad), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, bad); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := h.ClearLogs(nil, ClearLogsOptions{All: true, Yes: true}); err == nil || !strings.Contains(err.Error(), "app:") {
		t.Fatalf("failure did not propagate: %v", err)
	}
	assertLogSize(t, victim, false)
	assertLogSize(t, good, true)
	if !strings.Contains(out.String(), "clearing incomplete") || !strings.Contains(out.String(), "second: cleared 1 log files") {
		t.Fatal("partial clearing was not reported accurately")
	}
}
