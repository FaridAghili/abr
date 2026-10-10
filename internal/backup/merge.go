package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Merge joins checked app archives without accepting conflicting shared files.
// Full-server archives must be restored on their own.
func Merge(target *Manifest, incoming Manifest, source, destination string) error {
	if target.Files == nil {
		*target = incoming
		return CopyTree(source, destination)
	}
	if target.Scope != "app" || incoming.Scope != "app" {
		return fmt.Errorf("multiple archives must all be app backups")
	}
	if target.Settings != incoming.Settings || target.Config.Ports != incoming.Config.Ports {
		return fmt.Errorf("archives have different server settings")
	}
	combined := *target
	combined.Config.Apps = append(append(target.Config.Apps[:0:0], target.Config.Apps...), incoming.Config.Apps...)
	combined.Apps = append(append(target.Apps[:0:0], target.Apps...), incoming.Apps...)
	combined.Ports.Assignments = append(append(target.Ports.Assignments[:0:0], target.Ports.Assignments...), incoming.Ports.Assignments...)
	combined.Files = make(map[string]File, len(target.Files)+len(incoming.Files))
	for name, record := range target.Files {
		combined.Files[name] = record
	}
	for name, record := range incoming.Files {
		if previous, exists := combined.Files[name]; exists && previous != record {
			return fmt.Errorf("archives have conflicting shared file: %s", name)
		}
		combined.Files[name] = record
	}
	if err := combined.Validate(); err != nil {
		return err
	}
	for name, record := range incoming.Files {
		if _, exists := target.Files[name]; exists {
			continue
		}
		path := filepath.Join(destination, filepath.FromSlash(name))
		if record.Directory {
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := mergeFile(filepath.Join(source, filepath.FromSlash(name)), path); err != nil {
			return err
		}
	}
	*target = combined
	return nil
}

func mergeFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
