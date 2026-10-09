package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"abr/internal/services"
)

type journalRunner struct {
	calls []Command
}

func (r *journalRunner) Run(c Command) ([]byte, error) {
	r.calls = append(r.calls, c)
	return []byte("2026-10-09T12:00:00 app-service: first app\n2026-10-09T12:00:01 web-service: second app\n"), nil
}

func TestReadAllJournalLogsUsesOneCombinedScopedQuery(t *testing.T) {
	h, _, out, app := fixture(t)
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.saveManifest(manifest{Version: 1, App: app.Name, User: app.User, FPM: true, Units: []string{"abr-app-queue@1.service", "abr-app-scheduler.timer"}}); err != nil {
		t.Fatal(err)
	}
	second := app
	second.Name, second.User, second.Domain, second.Directory = "second", "abr-second", "second.localhost", filepath.Join(h.AppsDir, "second")
	if _, err := h.Manager.Register(second, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.saveManifest(manifest{Version: 1, App: second.Name, User: second.User, FPM: true, Units: []string{"abr-second-queue@1.service"}}); err != nil {
		t.Fatal(err)
	}
	runner := &journalRunner{}
	h.Runner = runner
	out.Reset()
	if err := h.ReadLogs("", "", ReadLogsOptions{All: true, Lines: 20}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Name != "journalctl" || !runner.calls[0].Private {
		t.Fatal("all-app view did not use one private journal snapshot")
	}
	args := runner.calls[0].Args
	for _, unit := range []string{"abr-app-queue@1.service", "abr-app-scheduler.service", "abr-second-queue@1.service", "php" + services.PHPVersion + "-fpm"} {
		if !slices.Contains(args, unit) {
			t.Fatalf("managed service omitted: %s", unit)
		}
	}
	if strings.Count(strings.Join(args, " "), "php"+services.PHPVersion+"-fpm") != 1 || !slices.Contains(args, "--output=with-unit") || !slices.Contains(args, "20") {
		t.Fatal("shared service was duplicated or journal settings were lost")
	}
	if !strings.Contains(out.String(), "first app") || !strings.Contains(out.String(), "second app") || !strings.Contains(out.String(), "PHP-FPM is shared") {
		t.Fatal("combined output or shared-pool scope note missing")
	}
	runner.calls = nil
	if err := h.ReadLogs(app.Name, "queue", ReadLogsOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0].Args, "abr-second-queue@1.service") || slices.Contains(runner.calls[0].Args, "php"+services.PHPVersion+"-fpm") {
		t.Fatal("single-app service filter included another service")
	}
}

func TestReadFileLogsForOneAndAllApps(t *testing.T) {
	h, runner, out, app := fixture(t)
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	second := app
	second.Name, second.User, second.Domain = "second", "abr-second", "second.localhost"
	second.Directory = filepath.Join(h.AppsDir, "second")
	if _, err := h.Manager.Register(second, nil); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(app.Directory, "storage/logs/laravel.log")
	other := filepath.Join(second.Directory, "logs/worker.log")
	upload := filepath.Join(app.Directory, "storage/app/public/upload.log")
	deploy := filepath.Join(h.Manager.StateDir, "deployments", app.Name, "run.log")
	for _, path := range []string{first, other, upload, deploy} {
		logFile(t, path)
	}
	if err := os.WriteFile(first, []byte("old first line\nnew first line\n"), 0640); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := h.ReadLogs(app.Name, "", ReadLogsOptions{Type: "application", Lines: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new first line") || strings.Contains(out.String(), "old first line") || strings.Contains(out.String(), other) || strings.Contains(out.String(), upload) || strings.Contains(out.String(), deploy) {
		t.Fatalf("file log scope or tail incorrect: %s", out.String())
	}
	out.Reset()
	if err := h.ReadLogs("", "", ReadLogsOptions{All: true, Type: "application"}); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"== app / application logs ==", "== second / application logs ==", first, other} {
		if !strings.Contains(out.String(), label) {
			t.Fatalf("all-app file view missing %s", label)
		}
	}
	out.Reset()
	if err := h.ReadLogs(app.Name, "", ReadLogsOptions{Type: "deployment"}); err != nil || !strings.Contains(out.String(), deploy) || strings.Contains(out.String(), first) {
		t.Fatalf("deployment log scope incorrect: %s %v", out.String(), err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("reading files executed host commands")
	}
	assertLogSize(t, first, false)
	assertLogSize(t, upload, false)
}

func TestReadLogsBoundsRecentFileSelection(t *testing.T) {
	h, _, out, app := fixture(t)
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"old", "third", "second", "latest"} {
		path := filepath.Join(app.Directory, "storage/logs", name+".log")
		logFile(t, path)
		stamp := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	latest := filepath.Join(app.Directory, "storage/logs/latest.log")
	if err := os.WriteFile(latest, []byte(strings.Repeat("older logs\n", 100000)+"NEWEST\n"), 0640); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := h.ReadLogs(app.Name, "", ReadLogsOptions{Type: "application", Lines: 1000}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NEWEST") || strings.Contains(out.String(), "old.log") || !strings.Contains(out.String(), "3 most recently modified") || out.Len() > 60*1024 {
		t.Fatal("viewer read old files or emitted unbounded content")
	}
}

func TestReadLogsRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			h, _, out, app := fixture(t)
			if _, err := h.Manager.Register(app, nil); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(h.root, "private.log")
			logFile(t, victim)
			if err := os.WriteFile(victim, []byte("PRIVATE OUTSIDE LOGS"), 0600); err != nil {
				t.Fatal(err)
			}
			logs := filepath.Join(app.Directory, "storage/logs")
			if err := os.MkdirAll(logs, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(logs, "unsafe.log")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(victim, path)
			case "hardlink":
				err = os.Link(victim, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				if err = os.Remove(logs); err == nil {
					err = os.Symlink(h.root, logs)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := h.ReadLogs(app.Name, "", ReadLogsOptions{Type: "application"}); err == nil || strings.Contains(out.String(), "PRIVATE OUTSIDE LOGS") {
				t.Fatal("unsafe log file was read")
			}
		})
	}
}

func TestReadLogsValidationAndMissingSources(t *testing.T) {
	h, runner, out, app := fixture(t)
	if _, err := h.Manager.Register(app, nil); err != nil {
		t.Fatal(err)
	}
	for _, o := range []ReadLogsOptions{{}, {All: true}, {Type: "unknown"}, {Type: "application", Follow: true}, {Lines: -1}, {Lines: 1001}} {
		name := app.Name
		if o == (ReadLogsOptions{}) {
			name = ""
		}
		if err := h.ReadLogs(name, "", o); err == nil {
			t.Fatalf("invalid selection accepted: %+v", o)
		}
	}
	if err := h.ReadLogs(app.Name, "web", ReadLogsOptions{Type: "application"}); err == nil {
		t.Fatal("file logs accepted a service selector")
	}
	if err := h.ReadLogs("", "", ReadLogsOptions{All: true}); err == nil || len(runner.calls) != 0 {
		t.Fatal("empty journal selection executed an unfiltered journal query")
	}
	out.Reset()
	if err := h.ReadLogs(app.Name, "", ReadLogsOptions{Type: "application"}); err != nil || !strings.Contains(out.String(), "No *.log files found") {
		t.Fatal("missing file logs did not report their absence")
	}
	h.DryRun = true
	out.Reset()
	if err := h.ReadLogs("", "", ReadLogsOptions{All: true, Type: "application"}); err != nil || !strings.Contains(out.String(), "Would read app") || len(runner.calls) != 0 {
		t.Fatal("file log preview performed reads or commands")
	}
	// A corrupt manifest must not expand --all to arbitrary host services.
	h.DryRun = false
	data, _ := json.Marshal(manifest{Version: 1, App: app.Name, User: app.User, Units: []string{"mysql.service"}})
	if err := h.write(h.manifestPath(app.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.ReadLogs("", "", ReadLogsOptions{All: true}); err == nil || len(runner.calls) != 0 {
		t.Fatal("unmanaged service was accepted")
	}
}
