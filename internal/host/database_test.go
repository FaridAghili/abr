package host

import (
	"strings"
	"testing"
)

func TestDatabaseNamesUseFullAppNamesWithoutAddedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, user string
	}{
		{"example-api", "example_api"},
		{"abr-example", "abr_example"},
		{strings.Repeat("a", 32), strings.Repeat("a", 32)},
		{strings.Repeat("a", 33), "aaaaaaaaaaaaaaaaaaaaaaa_852785c8"},
		{"long-" + strings.Repeat("a", 58), "long_aaaaaaaaaaaaaaaaaa_32a00b7d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, a := fixture(t)
			a.Name, a.Database.Enabled = tc.name, true
			if _, err := h.Register(a, nil); err != nil {
				t.Fatal(err)
			}
			c, err := h.transferCredentials(a)
			if err != nil {
				t.Fatal(err)
			}
			if c.Database != strings.ReplaceAll(tc.name, "-", "_") || c.User != tc.user {
				t.Fatalf("database = %q, user = %q; want user %q", c.Database, c.User, tc.user)
			}
			env, err := h.read(h.credentialsEnvPath(a.Name))
			if err != nil || dotenvValue(env, "DB_DATABASE") != c.Database || dotenvValue(env, "DB_USERNAME") != tc.user {
				t.Fatalf("credential environment naming mismatch: %v", err)
			}
			if err := h.Database(a.Name, false); err != nil {
				t.Fatalf("repeated provisioning: %v", err)
			}
			plan, err := h.planPurge(a)
			if err != nil || plan.database == nil || plan.database.User != tc.user {
				t.Fatalf("purge ownership naming mismatch: %v", err)
			}
		})
	}
}
