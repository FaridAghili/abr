package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeProc(t *testing.T, h Host, pid int, ticks uint64, unit string) {
	t.Helper()
	fields := make([]string, 49)
	for i := range fields {
		fields[i] = "0"
	}
	fields[14-4], fields[15-4], fields[20-4], fields[22-4], fields[24-4] = fmt.Sprint(ticks), "0", "2", "77", "10"
	dir := h.path(fmt.Sprintf("/proc/%d", pid))
	for name, data := range map[string]string{
		"stat":         fmt.Sprintf("%d (php-fpm: pool abr-app) S %s\n", pid, strings.Join(fields, " ")),
		"smaps_rollup": "Rss: 4096 kB\nPss: 2048 kB\n",
		"cgroup":       "0::/system.slice/" + unit + ".service\n",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageReportReadsProcForVerifiedAppUsers(t *testing.T) {
	h, _, out, a := fixture(t)
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	// Processes in the fixture are owned by the test user, so it stands in for the app user.
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("runtime users are never root; run as an ordinary user")
	}
	passwd := fmt.Sprintf("root:x:0:0:root:/root:/bin/bash\n%s:x:%d:%d:abr-%s:/var/lib/abr-users/%s:/usr/sbin/nologin\nmysql:x:%d:%d::/nonexistent:/bin/false\n", a.User, uid, uid, a.Name, a.User, uid+1, uid+1)
	for path, data := range map[string]string{
		"/etc/passwd":   passwd,
		"/proc/stat":    "cpu  100 0 0 900 0 0 0 0 0 0\ncpu0 100 0 0 900 0 0 0 0 0 0\n",
		"/proc/meminfo": "MemTotal: 1000000 kB\nMemAvailable: 500000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"/proc/loadavg": "0.10 0.20 0.30 1/100 5\n",
	} {
		if err := os.MkdirAll(filepath.Dir(h.path(path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.path(path), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, h, 41, 10, "php8.5-fpm")
	writeProc(t, h, 42, 30, "abr-app-queue@1")
	report, sample, err := h.UsageReport("", nil, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	app := report.Apps[0]
	if len(report.Apps) != 2 || app.Name != a.Name || !app.Known || app.Processes != 2 || app.MemoryBytes != 2*2048*1024 || app.Threads != 4 || report.Apps[1].Name != "MySQL" || report.Apps[1].Processes != 0 {
		t.Fatalf("report: %+v", report.Apps)
	}
	// The next refresh measures CPU since the returned sample.
	writeProc(t, h, 42, 80, "abr-app-queue@1")
	if err := os.WriteFile(h.path("/proc/stat"), []byte("cpu  200 0 0 1000 0 0 0 0 0 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	report, _, err = h.UsageReport(a.Name, &sample, 0)
	if err != nil || len(report.Apps) != 1 || report.Host.CPUPercent != 50 || report.Apps[0].CPUPercent != 25 || report.Apps[0].Services[0].Name == "" {
		t.Fatalf("refresh: %+v %v", report, err)
	}
	if _, _, err := h.UsageReport("missing", nil, 0); err == nil {
		t.Fatal("unknown app accepted")
	}
	// A same-named account that is not the dedicated runtime user is never attributed.
	if err := os.WriteFile(h.path("/etc/passwd"), []byte(fmt.Sprintf("%s:x:%d:%d:someone:/home/x:/bin/bash\n", a.User, uid, uid)), 0644); err != nil {
		t.Fatal(err)
	}
	if report, _, err := h.UsageReport(a.Name, nil, 0); err != nil || report.Apps[0].Known || report.Apps[0].Processes != 0 {
		t.Fatalf("unverified account attributed: %+v %v", report.Apps, err)
	}
	h.DryRun = true
	if _, _, err := h.UsageReport("", nil, 0); err == nil {
		t.Fatal("preview reported measurements")
	}
	if err := h.Usage("", UsageOptions{Interval: time.Second}); err != nil || !strings.Contains(out.String(), "Would read /proc") {
		t.Fatalf("preview: %v %s", err, out.String())
	}
}
