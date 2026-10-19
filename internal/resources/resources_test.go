package resources

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParsers(t *testing.T) {
	total, idle, cores, err := ParseCPU([]byte("cpu  100 0 50 800 50 0 0 0 20 0\ncpu0 50 0 25 400 25 0 0 0 10 0\ncpu1 50 0 25 400 25 0 0 0 10 0\nintr 1\n"))
	if err != nil || total != 1000 || idle != 850 || cores != 2 {
		t.Fatalf("cpu: %d %d %d %v", total, idle, cores, err)
	}
	if _, _, _, err := ParseCPU([]byte("intr 1\n")); err == nil {
		t.Fatal("missing cpu line accepted")
	}
	memory, available, swap, free, err := ParseMemory([]byte("MemTotal: 4000 kB\nMemFree: 100 kB\nMemAvailable: 1000 kB\nSwapTotal: 2000 kB\nSwapFree: 1500 kB\n"))
	if err != nil || memory != 4000*1024 || available != 1000*1024 || swap != 2000*1024 || free != 1500*1024 {
		t.Fatalf("memory: %d %d %d %d %v", memory, available, swap, free, err)
	}
	load, err := ParseLoad([]byte("0.50 0.25 0.10 1/200 999\n"))
	if err != nil || load != [3]float64{0.5, 0.25, 0.1} {
		t.Fatalf("load: %v %v", load, err)
	}
	// Command names may contain spaces and parentheses.
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[14-3], fields[15-3], fields[20-3], fields[22-3], fields[24-3] = "70", "30", "4", "12345", "256"
	stat, err := ParseProcessStat([]byte("42 (php-fpm: pool (abr) x) S " + strings.Join(fields[1:], " ")))
	if err != nil || stat.Ticks != 100 || stat.Threads != 4 || stat.Start != 12345 || stat.RSSPages != 256 {
		t.Fatalf("process stat: %+v %v", stat, err)
	}
	if _, err := ParseProcessStat([]byte("42 (short) S 1 2")); err == nil {
		t.Fatal("truncated process stat accepted")
	}
	if pss, ok := ParsePSS([]byte("Rss: 900 kB\nPss: 300 kB\n")); !ok || pss != 300*1024 {
		t.Fatalf("pss: %d %v", pss, ok)
	}
	for data, want := range map[string]string{
		"0::/system.slice/abr-shop-queue@1.service\n":                    "abr-shop-queue@1",
		"0::/system.slice/php8.5-fpm.service\n":                          "php8.5-fpm",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice\n":   "other",
		"0::/user.slice/user-1000.slice/session-3.scope\n":               "other",
		"12:cpu:/legacy\n0::/system.slice/abr-shop-octane.service/child": "abr-shop-octane",
	} {
		if got := ParseUnit([]byte(data)); got != want {
			t.Fatalf("unit %q = %q, want %q", data, got, want)
		}
	}
}

func TestBuildAttributesCPUAndMemoryPerAccount(t *testing.T) {
	start := time.Unix(1000, 0)
	accounts := []Account{{Name: "shop", Kind: "app", Type: "laravel", UID: 1001, Known: true}, {Name: "blog", Kind: "app", Type: "nuxt"}, {Name: "MySQL", Kind: "shared", UID: 110, Known: true}}
	previous := Sample{At: start, CPUTotal: 1000, CPUIdle: 800, Processes: map[ProcessKey]Process{
		{PID: 10, Start: 1}: {UID: 1001, Unit: "php8.5-fpm", Ticks: 50},
		{PID: 11, Start: 1}: {UID: 1001, Unit: "abr-shop-queue@1", Ticks: 500},
	}}
	current := Sample{At: start.Add(2 * time.Second), CPUTotal: 2000, CPUIdle: 1600, Cores: 2, MemoryTotal: 1000, Available: 400, SwapTotal: 100, SwapFree: 100, Processes: map[ProcessKey]Process{
		{PID: 10, Start: 1}: {UID: 1001, Unit: "php8.5-fpm", Ticks: 150, Memory: 100, Threads: 1},
		// PID 11 was reused by a new process; its full CPU time is new.
		{PID: 11, Start: 9}: {UID: 1001, Unit: "abr-shop-queue@1", Ticks: 50, Memory: 200, Threads: 2},
		{PID: 12, Start: 1}: {UID: 110, Unit: "mysql", Ticks: 0, Memory: 300, Threads: 30},
		{PID: 13, Start: 1}: {UID: 0, Unit: "caddy", Ticks: 999, Memory: 999},
	}}
	r := Build(accounts, previous, current)
	if r.Interval != 2*time.Second || r.Host.CPUPercent != 20 || r.Host.MemoryUsed != 600 || r.Host.MemoryPercent != 60 || r.Host.SwapUsed != 0 {
		t.Fatalf("host: %+v", r.Host)
	}
	shop, blog, mysql := r.Apps[0], r.Apps[1], r.Apps[2]
	if math.Abs(shop.CPUPercent-15) > 1e-9 || shop.MemoryBytes != 300 || shop.MemoryPercent != 30 || shop.Processes != 2 || shop.Threads != 3 {
		t.Fatalf("shop: %+v", shop)
	}
	if len(shop.Services) != 2 || shop.Services[0].Name != "abr-shop-queue@1" || shop.Services[0].MemoryBytes != 200 || shop.Services[1].CPUPercent != 10 {
		t.Fatalf("services: %+v", shop.Services)
	}
	if blog.Known || blog.Processes != 0 || mysql.MemoryBytes != 300 || mysql.Processes != 1 {
		t.Fatalf("other rows: %+v %+v", blog, mysql)
	}
	// Without a baseline only memory is meaningful.
	if first := Build(accounts, Sample{}, current); first.Apps[0].CPUPercent != 0 || first.Host.CPUPercent != 0 || first.Apps[0].MemoryBytes != 300 {
		t.Fatalf("first sample: %+v", first.Apps[0])
	}
}

func TestWriteTextAndJSON(t *testing.T) {
	r := Report{MeasuredAt: time.Unix(0, 0), Interval: time.Second, Host: Host{Cores: 2, MemoryTotal: 1 << 30}, Apps: []App{
		{Name: "shop", Kind: "app", Known: true, CPUPercent: 12.5, MemoryBytes: 64 << 20, MemoryPercent: 6.25, Processes: 3, Services: []Service{{Name: "php8.5-fpm", MemoryBytes: 64 << 20, Processes: 3}}},
	}, Notes: Notes}
	var text bytes.Buffer
	if err := Write(&text, r, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "shop", "12.5%", "64.0 MiB", "SERVICE", "php8.5-fpm", "PSS"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("missing %q:\n%s", want, text.String())
		}
	}
	var encoded bytes.Buffer
	if err := Write(&encoded, r, true); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil || decoded.Apps[0].MemoryBytes != 64<<20 {
		t.Fatalf("json: %v %s", err, encoded.String())
	}
}
