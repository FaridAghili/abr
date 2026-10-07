package abr

import (
	"bytes"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestDefaultsWorkWithoutSourceFiles(t *testing.T) {
	expected := map[string][]byte{}
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmpl") {
			data, err := os.ReadFile("templates/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			expected[entry.Name()] = data
		}
	}
	example, err := os.ReadFile("config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	templates, err := TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	bundled, err := fs.ReadDir(templates, ".")
	if err != nil || len(bundled) != len(expected) || len(bundled) == 0 {
		t.Fatalf("bundled templates: %d, expected %d: %v", len(bundled), len(expected), err)
	}
	for name, want := range expected {
		got, err := fs.ReadFile(templates, name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("embedded %s differs from source: %v", name, err)
		}
	}
	got, err := ExampleConfig()
	if err != nil || !bytes.Equal(got, example) {
		t.Fatalf("embedded example differs from source: %v", err)
	}
}
