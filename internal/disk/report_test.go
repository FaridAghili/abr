package disk

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseDUPreservesPathsAndRejectsIncompleteMeasurements(t *testing.T) {
	paths := []string{"/apps/one", "/apps/space and\nnewline\ttab"}
	output := []byte("4096\t" + paths[0] + "\x008192\t" + paths[1] + "\x00")
	sizes, err := ParseDU(output, paths)
	if err != nil || sizes[paths[0]] != 4096 || sizes[paths[1]] != 8192 {
		t.Fatalf("invalid summaries: %v %v", sizes, err)
	}
	for _, bad := range []string{"", "4096\t/apps/one\x00", "-1\t/apps/one\x00", "1\t/unknown\x00", "1\t/apps/one\x001\t/apps/one\x00", "oops\x00"} {
		if _, err := ParseDU([]byte(bad), paths); err == nil {
			t.Fatalf("accepted incomplete or invalid summary: %q", bad)
		}
	}
}

func TestDiskReportTextAndJSON(t *testing.T) {
	r := Report{MeasuredAt: time.Now().UTC(), Cached: true, FilesBytes: 1024, DatabaseBytes: 2048, TotalBytes: 3072,
		Apps:        []App{{Name: "api", ProjectBytes: 1024, FilesBytes: 1024, DatabaseBytes: 2048, DatabaseManaged: true, TotalBytes: 3072}},
		Filesystems: []Filesystem{{Path: "/", CapacityBytes: 8192, UsedBytes: 4096, AvailableBytes: 3072, UsedPercent: 50}}}
	var out bytes.Buffer
	if err := Write(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"cached; --refresh", "api", "DATABASE~", "App files: 1.0 KiB", "AVAILABLE", "50.0%"} {
		if !strings.Contains(out.String(), label) {
			t.Fatalf("missing report information %q: %s", label, out.String())
		}
	}
	out.Reset()
	if err := Write(&out, r, true); err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || parsed.TotalBytes != 3072 || parsed.Apps[0].ProjectBytes != 1024 || !parsed.Cached {
		t.Fatalf("JSON is not scriptable: %s %v", out.String(), err)
	}
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1 << 30: "1.0 GiB", 1 << 40: "1.0 TiB"} {
		if got := Bytes(n); got != want {
			t.Fatalf("format %d: %s, want %s", n, got, want)
		}
	}
}
