package host

import (
	"encoding/json"
	"testing"
)

func TestLatestRoadRunnerSelectsSupportedStableRelease(t *testing.T) {
	// A future major release (or a prerelease) must not break setup or
	// replace Octane's shared runtime.
	data := []byte(`[
		{"tag_name":"v4.0.0"},
		{"tag_name":"v3.2.0","prerelease":true},
		{"tag_name":"v3.3.0","draft":true},
		{"tag_name":"v3.0.9"},
		{"tag_name":"v3.1.10","assets":[{"name":"fixture","digest":"fixture-sha"}]},
		{"tag_name":"v3.1.9"},
		{"tag_name":"v2025.1.15"},
		{"tag_name":"v3.1.11-rc.1"},
		{"tag_name":"3.9.0"}
	]`)
	release, err := resolveRoadRunnerRelease(data, "latest")
	if err != nil || release.Tag != "v3.1.10" {
		t.Fatalf("selected %s: %v", release.Tag, err)
	}
	if len(release.Assets) != 1 || release.Assets[0].Digest != "fixture-sha" {
		t.Fatal("lost selected release asset metadata")
	}
}

func TestLatestRoadRunnerRejectsMissingSupportedRelease(t *testing.T) {
	for _, data := range []string{
		`[]`,
		`[{"tag_name":"v2025.1.15"}]`,
		`[{"tag_name":"v4.0.0"}]`,
		`[{"tag_name":"v3.0.1","draft":true}]`,
		`[{"tag_name":"v3.0.1","prerelease":true}]`,
		`[{"tag_name":"v3.0.999999999999999999999999"}]`,
		`{"tag_name":"v3.0.1"}`,
		`invalid JSON`,
	} {
		if _, err := resolveRoadRunnerRelease([]byte(data), "latest"); err == nil {
			t.Fatalf("accepted unsupported release list: %s", data)
		}
	}
}

func TestPinnedRoadRunnerRequiresExactStableRelease(t *testing.T) {
	for _, test := range []struct {
		requested, tag    string
		draft, prerelease bool
		valid             bool
	}{
		{requested: "3.0.1", tag: "v3.0.1", valid: true},
		{requested: "3.0.1", tag: "v3.0.0"},
		{requested: "3.0.1", tag: "v3.0.1", draft: true},
		{requested: "3.0.1", tag: "v3.0.1", prerelease: true},
		{requested: "3.0.1", tag: "v3.0.1-rc.1"},
		{requested: "2025.1.15", tag: "v2025.1.15"},
		{requested: "4.0.0", tag: "v4.0.0"},
	} {
		data, err := json.Marshal(roadRunnerRelease{Tag: test.tag, Draft: test.draft, Prerelease: test.prerelease})
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveRoadRunnerRelease(data, test.requested)
		if (err == nil) != test.valid {
			t.Fatalf("requested %s tag %s draft=%v prerelease=%v: %v", test.requested, test.tag, test.draft, test.prerelease, err)
		}
	}
}
