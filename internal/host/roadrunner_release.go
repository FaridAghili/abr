package host

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

const roadRunnerReleasesURL = "https://api.github.com/repos/roadrunner-server/roadrunner/releases?per_page=100"

// roadRunnerVersionPattern accepts the RoadRunner 3.x releases Octane supports.
var roadRunnerVersionPattern = regexp.MustCompile(`^3\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

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

// Octane supports RoadRunner 3.x with the roadrunner/* PHP packages. A future
// incompatible major release must not replace the shared runtime merely
// because GitHub marks it latest. Select the highest stable 3.x release.
func resolveRoadRunnerRelease(data []byte, requested string) (roadRunnerRelease, error) {
	var selected roadRunnerRelease
	if requested != "latest" {
		if err := json.Unmarshal(data, &selected); err != nil {
			return selected, err
		}
		if selected.Draft || selected.Prerelease || selected.Tag != "v"+requested || !roadRunnerVersionPattern.MatchString(requested) {
			return roadRunnerRelease{}, fmt.Errorf("unexpected or unstable RoadRunner release")
		}
		return selected, nil
	}
	var releases []roadRunnerRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return selected, err
	}
	bestMinor, bestPatch := -1, -1
	for _, release := range releases {
		if release.Draft || release.Prerelease || len(release.Tag) < 2 || release.Tag[0] != 'v' {
			continue
		}
		match := roadRunnerVersionPattern.FindStringSubmatch(release.Tag[1:])
		if match == nil {
			continue
		}
		minor, minorErr := strconv.Atoi(match[1])
		patch, patchErr := strconv.Atoi(match[2])
		if minorErr != nil || patchErr != nil || minor < bestMinor || (minor == bestMinor && patch <= bestPatch) {
			continue
		}
		selected, bestMinor, bestPatch = release, minor, patch
	}
	if bestMinor < 0 {
		return selected, fmt.Errorf("no stable Octane-compatible RoadRunner 3.x release found")
	}
	return selected, nil
}
