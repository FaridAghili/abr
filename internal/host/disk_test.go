package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"abr/internal/disk"
)

type diskRunner struct {
	calls      []Command
	sizes      map[string]uint64
	databases  string
	fail       bool
	incomplete bool
	delay      time.Duration
}

func (r *diskRunner) Run(c Command) ([]byte, error) {
	r.calls = append(r.calls, c)
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if r.fail {
		return nil, fmt.Errorf("measurement failed")
	}
	if c.Name == "du" {
		var out bytes.Buffer
		for _, path := range strings.Split(string(c.Input), "\x00") {
			if path != "" && !r.incomplete {
				fmt.Fprintf(&out, "%d\t%s\x00", r.sizes[path], path)
			}
		}
		return out.Bytes(), nil
	}
	if c.Name == "mysql" {
		return []byte(r.databases), nil
	}
	return nil, fmt.Errorf("unexpected measurement command %s", c.Name)
}

func diskFixture(t *testing.T) (Host, *diskRunner, *bytes.Buffer) {
	t.Helper()
	h, _, out, a := fixture(t)
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	home := "/var/lib/abr-users/" + a.User
	user, _ := json.Marshal(userRecord{App: a.Name, User: a.User, Home: home, UID: "991", GID: "991"})
	if err := h.write(h.userPath(a), user, 0600); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(credentials{App: a.Name, Database: databaseName(a.Name), User: databaseUser(a.Name), Ready: true})
	if err := h.write(h.credentialsPath(a.Name), record, 0600); err != nil {
		t.Fatal(err)
	}
	deploy := h.path(filepath.Join(h.Manager.StateDir, "deployments", a.Name))
	if err := os.MkdirAll(deploy, 0700); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Name, b.User, b.Domain, b.Type, b.Web.Driver = "web", "abr-web", "web.localhost", "nuxt", ""
	b.Directory = filepath.Join(h.AppsDir, "web")
	if err := os.MkdirAll(b.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Manager.Register(b, nil); err != nil {
		t.Fatal(err)
	}
	runner := &diskRunner{sizes: map[string]uint64{a.Directory: 10000, h.path(home): 2000, deploy: 3000, b.Directory: 4000}, databases: "app\t5000\n"}
	h.Runner = runner
	out.Reset()
	return h, runner, out
}

func diskReport(t *testing.T, h Host, out *bytes.Buffer, name string, refresh bool) disk.Report {
	t.Helper()
	out.Reset()
	if err := h.DiskUsage(name, DiskOptions{JSON: true, Refresh: refresh}); err != nil {
		t.Fatal(err)
	}
	var report disk.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %s %v", out.String(), err)
	}
	return report
}

func TestDiskUsageBatchesAndCachesScansWithLiveFilesystems(t *testing.T) {
	h, runner, out := diskFixture(t)
	r := diskReport(t, h, out, "", false)
	if r.Cached || len(r.Apps) != 2 || r.Apps[0].Name != "app" || r.Apps[0].ProjectBytes != 10000 || r.Apps[0].HomeBytes != 2000 || r.Apps[0].DeploymentBytes != 3000 || r.Apps[0].DatabaseBytes != 5000 || r.TotalBytes != 24000 || r.FilesBytes != 19000 {
		t.Fatalf("incorrect app accounting: %+v", r)
	}
	if len(r.Filesystems) != 1 || r.Filesystems[0].CapacityBytes == 0 || r.Filesystems[0].AvailableBytes > r.Filesystems[0].CapacityBytes {
		t.Fatalf("filesystem stats missing or counted twice: %+v", r.Filesystems)
	}
	if len(runner.calls) != 2 || runner.calls[0].Name != "du" || runner.calls[1].Name != "mysql" {
		t.Fatalf("expected one file scan and one metadata query: %v", runner.calls)
	}
	for _, c := range runner.calls {
		if !c.Private || strings.Contains(strings.Join(c.Args, " "), "SELECT") {
			t.Fatal("measurement exposed private SQL or raw command output")
		}
	}
	runner.sizes[filepath.Join(h.AppsDir, "app")] += 1000
	cached := diskReport(t, h, out, "", false)
	if !cached.Cached || cached.TotalBytes != r.TotalBytes || len(runner.calls) != 2 || !cached.MeasuredAt.Equal(r.MeasuredAt) || len(cached.Filesystems) != 1 {
		t.Fatal("repeat request rescanned files or omitted live filesystem stats")
	}
	fresh := diskReport(t, h, out, "", true)
	if fresh.Cached || fresh.TotalBytes != r.TotalBytes+1000 || len(runner.calls) != 4 {
		t.Fatal("explicit refresh did not rescan")
	}
	one := diskReport(t, h, out, "web", false)
	if len(one.Apps) != 1 || one.TotalBytes != 4000 || len(runner.calls) != 5 {
		t.Fatal("single-app report scanned another app or queried unrelated databases")
	}
	if strings.Contains(string(runner.calls[4].Input), filepath.Join(h.AppsDir, "app")) {
		t.Fatal("single-app request scanned unrelated paths")
	}
}

func TestDiskUsageCacheExpiryAndConfigurationChanges(t *testing.T) {
	h, runner, out := diskFixture(t)
	diskReport(t, h, out, "", false)
	path := filepath.Join(h.Manager.StateDir, "disk-usage/all.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cache diskCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	cache.Report.MeasuredAt = time.Now().Add(-2 * diskCacheTTL)
	data, _ = json.Marshal(cache)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if r := diskReport(t, h, out, "", false); r.Cached || len(runner.calls) != 4 {
		t.Fatal("expired cache was served")
	}
	if err := h.Manager.Remove("web"); err != nil {
		t.Fatal(err)
	}
	if r := diskReport(t, h, out, "", false); r.Cached || len(r.Apps) != 1 || len(runner.calls) != 6 {
		t.Fatal("changed configuration reused old app totals")
	}
}

func TestConcurrentDiskViewsShareOneScan(t *testing.T) {
	h, runner, _ := diskFixture(t)
	runner.delay = 50 * time.Millisecond
	start := make(chan struct{})
	errors := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			requestHost := h
			requestHost.Output = &bytes.Buffer{}
			errors <- requestHost.DiskUsage("", DiskOptions{JSON: true})
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.calls) != 2 || runner.calls[0].Name != "du" || runner.calls[1].Name != "mysql" {
		t.Fatal("concurrent views multiplied file scans or database queries")
	}
}

func TestDiskUsageDoesNotClaimFailedScansAsSuccess(t *testing.T) {
	for _, failure := range []string{"command", "incomplete", "database"} {
		t.Run(failure, func(t *testing.T) {
			h, runner, out := diskFixture(t)
			switch failure {
			case "command":
				runner.fail = true
			case "incomplete":
				runner.incomplete = true
			case "database":
				runner.databases = "unrelated\t1000\n"
			}
			if err := h.DiskUsage("", DiskOptions{JSON: true}); err == nil || out.Len() != 0 {
				t.Fatalf("failed scan produced a successful report: %s %v", out.String(), err)
			}
			if _, err := os.Stat(filepath.Join(h.Manager.StateDir, "disk-usage/all.json")); !os.IsNotExist(err) {
				t.Fatal("failed measurement was cached")
			}
		})
	}
}

func TestDiskFilesystemOnlyAndPreviewAvoidAppScans(t *testing.T) {
	h, runner, out := diskFixture(t)
	if err := os.Remove(h.Manager.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := h.DiskUsage("", DiskOptions{JSON: true, FilesystemOnly: true}); err != nil || len(runner.calls) != 0 {
		t.Fatalf("filesystem-only required apps or scanned files: %v", err)
	}
	var report disk.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Filesystems) != 1 || len(report.Apps) != 0 || !report.FilesystemOnly || report.FilesystemMeasuredAt.IsZero() {
		t.Fatal("filesystem-only report is invalid")
	}
	if _, err := os.Stat(filepath.Join(h.Manager.StateDir, "disk-usage")); !os.IsNotExist(err) {
		t.Fatal("filesystem-only created app cache state")
	}
	h.DryRun = true
	out.Reset()
	if err := h.DiskUsage("", DiskOptions{FilesystemOnly: true}); err != nil || !strings.Contains(out.String(), "Would read live filesystem") || len(runner.calls) != 0 {
		t.Fatal("preview performed measurements")
	}
	for _, o := range []DiskOptions{{All: true}, {FilesystemOnly: true}} {
		if err := h.DiskUsage("app", o); err == nil {
			t.Fatal("conflicting selection accepted")
		}
	}
}

func TestDiskUsageRejectsTopLevelSymlinks(t *testing.T) {
	h, runner, out := diskFixture(t)
	project := filepath.Join(h.AppsDir, "web")
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.root, project); err != nil {
		t.Fatal(err)
	}
	if err := h.DiskUsage("web", DiskOptions{}); err == nil || len(runner.calls) != 0 || out.Len() != 0 {
		t.Fatal("top-level symlink was scanned or claimed as app usage")
	}
}
