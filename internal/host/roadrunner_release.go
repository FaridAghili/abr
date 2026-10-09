package host

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

const roadRunnerReleasesURL = "https://api.github.com/repos/roadrunner-server/roadrunner/releases?per_page=100"

type roadRunnerRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// Octane's current PHP integration uses RoadRunner's 2025.1 protocol family.
// A new incompatible major release must not replace the shared runtime merely
// because GitHub marks it latest. Select the highest stable supported patch.
func resolveRoadRunnerRelease(data []byte, requested string) (roadRunnerRelease, error) {
	var selected roadRunnerRelease
	if requested != "latest" {
		if err := json.Unmarshal(data, &selected); err != nil {
			return selected, err
		}
		if selected.Draft || selected.Prerelease || selected.Tag != "v"+requested || !regexp.MustCompile(`^v\d{4}\.\d+\.\d+$`).MatchString(selected.Tag) {
			return roadRunnerRelease{}, fmt.Errorf("unexpected or unstable RoadRunner release")
		}
		return selected, nil
	}
	var releases []roadRunnerRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return selected, err
	}
	pattern := regexp.MustCompile(`^v2025\.1\.(0|[1-9][0-9]*)$`)
	best := -1
	for _, release := range releases {
		match := pattern.FindStringSubmatch(release.Tag)
		if release.Draft || release.Prerelease || match == nil {
			continue
		}
		patch, err := strconv.Atoi(match[1])
		if err != nil || patch <= best {
			continue
		}
		selected, best = release, patch
	}
	if best < 0 {
		return selected, fmt.Errorf("no stable Octane-compatible RoadRunner 2025.1 release found")
	}
	return selected, nil
}
