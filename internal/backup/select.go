package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// App backups are published as NAME-YYYYMMDDHHMMSS.tar.gz.
var appArchiveName = regexp.MustCompile(`^(.+)-([0-9]{14})\.tar\.gz$`)

// LatestArchives selects the newest app archive of each app in directory, so a
// copy of the backup server's directory with many generations restores as-is.
func LatestArchives(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	latest := map[string]string{}
	for _, entry := range entries {
		match := appArchiveName.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() {
			continue
		}
		// One app's names differ only in their fixed-width UTC timestamps.
		if previous, ok := latest[match[1]]; !ok || entry.Name() > previous {
			latest[match[1]] = entry.Name()
		}
	}
	if len(latest) == 0 {
		return nil, fmt.Errorf("no app backups named NAME-YYYYMMDDHHMMSS.tar.gz in %s", directory)
	}
	files := make([]string, 0, len(latest))
	for _, name := range latest {
		files = append(files, filepath.Join(directory, name))
	}
	slices.Sort(files)
	return files, nil
}

// Expired returns app's archives beyond the newest keep, oldest first. Only
// exact NAME-YYYYMMDDHHMMSS.tar.gz names count, so other apps' archives and
// unrelated files are never selected. protect is never returned; keep 0
// keeps everything.
func Expired(names []string, app string, keep int, protect string) []string {
	if keep <= 0 {
		return nil
	}
	var archives []string
	for _, name := range names {
		if match := appArchiveName.FindStringSubmatch(name); match != nil && match[1] == app {
			archives = append(archives, name)
		}
	}
	slices.Sort(archives)
	var expired []string
	for _, name := range archives[:max(len(archives)-keep, 0)] {
		if name != protect {
			expired = append(expired, name)
		}
	}
	return expired
}
