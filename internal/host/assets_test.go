package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
