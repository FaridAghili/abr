package host

import (
	"encoding/json"
	"testing"
)

func TestLatestRoadRunnerSelectsSupportedStablePatch(t *testing.T) {
	// GitHub's latest release is now v3.0.0. Publishing that release (or a
	// prerelease) must not break setup or replace Octane's shared runtime.
	data := []byte(`[
		{"tag_name":"v3.0.0"},
		{"tag_name":"v2025.1.16","prerelease":true},
		{"tag_name":"v2025.1.18","draft":true},
		{"tag_name":"v2025.1.9"},
		{"tag_name":"v2025.1.15","assets":[{"name":"fixture","digest":"fixture-sha"}]},
		{"tag_name":"v2025.1.14"},
		{"tag_name":"v2026.1.0"},
		{"tag_name":"v2025.1.17-rc.1"}
	]`)
	release, err := resolveRoadRunnerRelease(data, "latest")
	if err != nil || release.Tag != "v2025.1.15" {
		t.Fatalf("selected %s: %v", release.Tag, err)
	}
	if len(release.Assets) != 1 || release.Assets[0].Digest != "fixture-sha" {
		t.Fatal("lost selected release asset metadata")
	}
}

func TestLatestRoadRunnerRejectsMissingSupportedRelease(t *testing.T) {
	for _, data := range []string{
		`[]`,
		`[{"tag_name":"v3.0.0"}]`,
		`[{"tag_name":"v2025.1.15","draft":true}]`,
		`[{"tag_name":"v2025.1.15","prerelease":true}]`,
		`[{"tag_name":"v2025.1.999999999999999999999999"}]`,
		`{"tag_name":"v2025.1.15"}`,
		`invalid JSON`,
	} {
		if _, err := resolveRoadRunnerRelease([]byte(data), "latest"); err == nil {
			t.Fatalf("accepted unsupported release list: %s", data)
		}
	}
}

func TestPinnedRoadRunnerRequiresExactStableRelease(t *testing.T) {
	for _, test := range []struct {
		tag               string
		draft, prerelease bool
		valid             bool
	}{
		{tag: "v2025.1.15", valid: true},
		{tag: "v2025.1.14"},
		{tag: "v2025.1.15", draft: true},
		{tag: "v2025.1.15", prerelease: true},
		{tag: "v2025.1.15-rc.1"},
		{tag: "v3.0.0"},
	} {
		data, err := json.Marshal(roadRunnerRelease{Tag: test.tag, Draft: test.draft, Prerelease: test.prerelease})
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveRoadRunnerRelease(data, "2025.1.15")
		if (err == nil) != test.valid {
			t.Fatalf("tag %s draft=%v prerelease=%v: %v", test.tag, test.draft, test.prerelease, err)
		}
	}
}
