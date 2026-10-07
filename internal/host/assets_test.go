package host

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAssetCompressionBatchesFilesAndStopsOnFailure(t *testing.T) {
	h, runner, _, a := fixture(t)
	directory := filepath.Join(a.Directory, "public/build/assets")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for i := range 130 {
		path := filepath.Join(directory, fmt.Sprintf("asset-%03d.js", i))
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 512)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.compressAssets(a, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected three batches, got %d", len(runner.calls))
	}
	count := 0
	for _, c := range runner.calls {
		index := slices.Index(c.Args, "--quality=5")
		paths := c.Args[index+2:]
		if len(paths) > 64 || c.Name != "runuser" || c.Args[1] != a.User {
			t.Fatal("unsafe compression batch", c)
		}
		count += len(paths)
	}
	if count != 130 {
		t.Fatal("assets skipped or duplicated", count)
	}
	runner.calls = nil
	runner.fail = func(Command) error { return testExit(1) }
	if err := h.compressAssets(a, nil); err == nil || len(runner.calls) != 1 {
		t.Fatal("continued after batch failure", err)
	}
}

func TestAssetCompressionRunsAsAppAndSkipsUnsafeFiles(t *testing.T) {
	h, runner, _, a := fixture(t)
	directory := filepath.Join(a.Directory, "public/build/assets")
	os.MkdirAll(directory, 0755)
	path := filepath.Join(directory, "app-a1b2c3d4.js")
	os.WriteFile(path, []byte(strings.Repeat("x", 1024)), 0644)
	os.WriteFile(filepath.Join(directory, "small.css"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(directory, "private.php"), []byte(strings.Repeat("x", 1024)), 0644)
	os.Symlink(path, filepath.Join(directory, "link.js"))
	if err := h.compressAssets(a, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatal("compressed unexpected files", runner.calls)
	}
	command := runner.calls[0]
	if command.Name != "runuser" || !slices.Contains(command.Args, a.User) || !slices.Contains(command.Args, "/usr/bin/brotli") || command.Args[len(command.Args)-1] != path {
		t.Fatal("compression escaped app user", command)
	}
	os.Symlink(filepath.Join(a.Directory, ".env"), path+".br")
	if err := h.compressAssets(a, nil); err == nil {
		t.Fatal("compressed sidecar symlink accepted")
	}
	os.RemoveAll(directory)
	os.Symlink(filepath.Join(a.Directory, "public"), directory)
	if err := h.compressAssets(a, nil); err == nil {
		t.Fatal("asset directory symlink accepted")
	}
}
