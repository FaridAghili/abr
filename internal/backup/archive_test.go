package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
)

func archiveFixture(t *testing.T) ([]byte, Manifest) {
	t.Helper()
	stage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stage, "apps/demo/storage/app/public/empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "apps/demo/.env"), []byte("secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, CreatedAt: time.Now().UTC(), Config: config.Default(), Ports: ports.Empty(), Apps: []App{}, Settings: Settings{Hostname: "backup-host", RoadRunnerVersion: "latest", NoRedis: true}}
	var out bytes.Buffer
	if err := Write(&out, stage, &m); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), m
}

func TestArchiveRoundTripAndCorruption(t *testing.T) {
	data, _ := archiveFixture(t)
	target := t.TempDir()
	m, err := Extract(bytes.NewReader(data), target)
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(target, "apps/demo/.env"))
	if err != nil || string(env) != "secret-value\n" {
		t.Fatal("lost secret file", err)
	}
	if !m.Files["apps/demo/storage/app/public/empty"].Directory {
		t.Fatal("lost empty upload directory")
	}
	info, _ := os.Stat(filepath.Join(target, "apps/demo/.env"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("extracted secrets are not private")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	gz.Close()
	raw = bytes.Replace(raw, []byte("secret-value"), []byte("broken-value"), 1)
	var corrupted bytes.Buffer
	writer := gzip.NewWriter(&corrupted)
	writer.Write(raw)
	writer.Close()
	for name, archive := range map[string][]byte{"checksum": corrupted.Bytes(), "truncated": data[:len(data)-5], "empty": {}, "gzip-footer": append(append([]byte{}, data[:len(data)-1]...), data[len(data)-1]^1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Extract(bytes.NewReader(archive), t.TempDir()); err == nil {
				t.Fatal("corrupt archive accepted")
			}
		})
	}
}

func TestArchiveRoundTripWithLargeInventory(t *testing.T) {
	_, m := archiveFixture(t)
	stage := t.TempDir()
	// Long but valid upload paths cross the former 8 MiB manifest cap without
	// needing gigabytes of payload or tens of thousands of filesystem entries.
	directory := strings.TrimSuffix(strings.Repeat(strings.Repeat("u", 200)+"/", 8), "/")
	if err := os.MkdirAll(filepath.Join(stage, filepath.FromSlash(directory)), 0700); err != nil {
		t.Fatal(err)
	}
	const files = 5000
	for i := range files {
		name := filepath.Join(stage, filepath.FromSlash(directory), fmt.Sprintf("upload-%04d", i))
		if err := os.WriteFile(name, []byte("upload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Write(&out, stage, &m); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 8<<20 {
		t.Fatalf("fixture must exceed the former 8 MiB cap, got %d bytes", len(data))
	}
	target := t.TempDir()
	restored, err := Extract(&out, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Files) != files+8 {
		t.Fatalf("lost inventory entries: got %d, want %d", len(restored.Files), files+8)
	}
	for name, record := range m.Files {
		if restored.Files[name] != record {
			t.Fatalf("lost metadata or checksum for %s", name)
		}
	}
	payload, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(directory), "upload-4999"))
	if err != nil || string(payload) != "upload" {
		t.Fatalf("lost upload payload: %q, %v", payload, err)
	}
}

func TestArchiveRejectsOversizedManifest(t *testing.T) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	// With no preceding inventory the metadata allowance still bounds the
	// manifest. Reject the declared size before attempting to read its payload.
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0600, Typeflag: tar.TypeReg, Size: maxManifestMetadata + 1}); err != nil {
		t.Fatal(err)
	}
	// The missing body is intentional; extraction must reject the header first.
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(&out, t.TempDir()); err == nil || err.Error() != "invalid backup manifest entry" {
		t.Fatalf("oversized manifest was not rejected at its header: %v", err)
	}
}

func TestArchiveRejectsUnsafeEntries(t *testing.T) {
	cases := []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "/absolute", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "a/../../escape", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "a\\escape", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0600},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0600},
		{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0600},
		{Name: "setuid", Typeflag: tar.TypeReg, Mode: 04755},
	}
	for _, header := range cases {
		t.Run(header.Name, func(t *testing.T) {
			var out bytes.Buffer
			gz := gzip.NewWriter(&out)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			tw.Close()
			gz.Close()
			if _, err := Extract(&out, t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestArchiveRejectsDuplicatesAndUnlistedFiles(t *testing.T) {
	data, _ := archiveFixture(t)
	reader, _ := gzip.NewReader(bytes.NewReader(data))
	tr := tar.NewReader(reader)
	for _, duplicate := range []bool{true, false} {
		var out bytes.Buffer
		gz := gzip.NewWriter(&out)
		tw := tar.NewWriter(gz)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := io.ReadAll(tr)
			if header.Name == "manifest.json" {
				if !duplicate {
					tw.WriteHeader(&tar.Header{Name: "unlisted", Mode: 0600, Typeflag: tar.TypeReg})
				}
			}
			tw.WriteHeader(header)
			tw.Write(payload)
			if duplicate && header.Name == "apps/demo/.env" {
				tw.WriteHeader(header)
				tw.Write(payload)
			}
		}
		tw.Close()
		gz.Close()
		if _, err := Extract(&out, t.TempDir()); err == nil {
			t.Fatal("duplicate/unlisted file accepted")
		}
		reader.Close()
		reader, _ = gzip.NewReader(bytes.NewReader(data))
		tr = tar.NewReader(reader)
	}
	reader.Close()
}

func TestCopyAndWriteRejectSourceLinks(t *testing.T) {
	_, m := archiveFixture(t)
	source := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "secret")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Write(&out, source, &m); err == nil {
		t.Fatal("source link accepted")
	}
	if err := CopyTree(source, t.TempDir()); err == nil {
		t.Fatal("copied source symlink")
	}
	source = t.TempDir()
	target := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(source, "file"), []byte("data"), 0600)
	os.Symlink(filepath.Join(outside, "untouched"), filepath.Join(target, "file"))
	if err := CopyTree(source, target); err == nil {
		t.Fatal("wrote through target link")
	}
	if _, err := os.Stat(filepath.Join(outside, "untouched")); !os.IsNotExist(err) {
		t.Fatal("escaped target")
	}
	if strings.Contains(out.String(), "root:") {
		t.Fatal("read linked secret")
	}
}
