package host

import (
	"strings"
	"testing"
)

func TestDatabaseNamesUseFullAppNamesWithoutAddedPrefix(t *testing.T) {
	for _, name := range []string{"example-api", "abr-example", "long-" + strings.Repeat("a", 58)} {
		t.Run(name, func(t *testing.T) {
			h, _, _, a := fixture(t)
			a.Name, a.Database.Enabled = name, true
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			c, err := h.transferCredentials(a)
			if err != nil || c.Database != strings.ReplaceAll(name, "-", "_") || c.User != RuntimeUser(name) {
				t.Fatalf("database or transfer naming mismatch: %v", err)
			}
		})
	}
}
