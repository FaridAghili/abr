package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renameFixture(t *testing.T) (Manifest, string) {
	t.Helper()
	m, stage := mergeFixture(t, "demo", 10001)
	for name, data := range map[string]string{
		"apps/demo/.env":                "DB_DATABASE=demo\n",
		"apps/demo/source/app.txt":      "source",
		"apps/demo/storage/app/a.txt":   "upload",
		"sql/demo.sql":                  "CREATE TABLE t (id INT);\n",
		"state/databases/demo.json":     `{"app":"demo"}`,
		"state/backups/demo.json":       `{"LastAttempt":"2026-10-10T02:00:00Z"}`,
		"apps/demonstration/source/x":   "not this app",
		"state/databases/demo-two.json": "not this app",
		"shared/Caddyfile":              "shared",
	} {
		path := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Write(&out, stage, &m, nil); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	extracted, err := Extract(bytes.NewReader(out.Bytes()), target)
	if err != nil {
		t.Fatal(err)
	}
	return extracted, target
}

func TestRenameAppMovesEveryEntryAndRecord(t *testing.T) {
	m, stage := renameFixture(t)
	if err := RenameApp(&m, stage, "demo-old"); err != nil {
		t.Fatal(err)
	}
	if m.Config.Apps[0].Name != "demo-old" || m.Apps[0].Name != "demo-old" || m.Ports.Assignments[0].App != "demo-old" {
		t.Fatal("app records kept the old name", m.Config.Apps, m.Apps, m.Ports)
	}
	for name := range m.Files {
		if name == "apps/demo" || strings.HasPrefix(name, "apps/demo/") || name == "sql/demo.sql" || name == "state/databases/demo.json" {
			t.Fatal("inventory kept an old entry", name)
		}
	}
	for name, want := range map[string]string{
		"apps/demo-old/.env":              "DB_DATABASE=demo\n",
		"apps/demo-old/source/app.txt":    "source",
		"apps/demo-old/storage/app/a.txt": "upload",
		"sql/demo-old.sql":                "CREATE TABLE t (id INT);\n",
		"state/databases/demo-old.json":   `{"app":"demo"}`,
		"state/backups/demo-old.json":     `{"LastAttempt":"2026-10-10T02:00:00Z"}`,
		"apps/demonstration/source/x":     "not this app",
		"state/databases/demo-two.json":   "not this app",
		"shared/Caddyfile":                "shared",
	} {
		if _, ok := m.Files[name]; !ok {
			t.Fatal("inventory lost entry", name)
		}
		if data, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(name))); err != nil || string(data) != want {
			t.Fatal("staged entry not moved", name, err)
		}
	}
	for _, name := range []string{"apps/demo", "sql/demo.sql", "state/databases/demo.json", "state/backups/demo.json"} {
		if _, err := os.Lstat(filepath.Join(stage, name)); !os.IsNotExist(err) {
			t.Fatal("old staged entry left behind", name)
		}
	}
	if err := ReplaceFile(&m, stage, "apps/demo-old/.env", []byte("DB_DATABASE=demo_old\n")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("DB_DATABASE=demo_old\n"))
	if record := m.Files["apps/demo-old/.env"]; record.Size != int64(len("DB_DATABASE=demo_old\n")) || record.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("replaced file record not updated", record)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(&m, stage, "apps/demo-old/source", nil); err == nil {
		t.Fatal("replaced a directory entry")
	}
}

func TestRenameAppRefusesInvalidAndAmbiguousRenames(t *testing.T) {
	m, stage := renameFixture(t)
	for _, name := range []string{"", "Demo", "-demo", "demo_old", strings.Repeat("a", 64)} {
		if err := RenameApp(&m, stage, name); err == nil {
			t.Fatal("invalid name accepted", name)
		}
	}
	if err := RenameApp(&m, stage, "demo"); err != nil || m.Config.Apps[0].Name != "demo" {
		t.Fatal("same name changed the archive", err)
	}
	// An existing staged destination must never be replaced.
	if err := os.MkdirAll(filepath.Join(stage, "apps/taken"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := RenameApp(&m, stage, "taken"); err == nil {
		t.Fatal("renamed over an existing staged entry")
	}
	two := m
	two.Config.Apps = append(append(two.Config.Apps[:0:0], m.Config.Apps...), m.Config.Apps[0])
	if err := RenameApp(&two, stage, "other"); err == nil || !strings.Contains(err.Error(), "single app") {
		t.Fatal("renamed a multi-app archive", err)
	}
}
