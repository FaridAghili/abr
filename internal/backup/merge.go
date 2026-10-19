package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// Merge joins checked app archives. App entries never overlap, so differing
// entries are shared server files and settings; scheduled app backups are taken
// at different times, so the newest archive's versions win. Full-server
// archives must be restored on their own. Entries taken from incoming are moved
// out of source, so source must be a private, disposable extraction.
func Merge(target *Manifest, incoming Manifest, source, destination string) error {
	combined := incoming
	replaced := map[string]bool{}
	if target.Files != nil {
		if target.Scope != "app" || incoming.Scope != "app" {
			return fmt.Errorf("multiple archives must all be app backups")
		}
		newer := incoming.CreatedAt.After(target.CreatedAt)
		combined = *target
		if newer {
			combined.CreatedAt, combined.Settings, combined.Config.Ports = incoming.CreatedAt, incoming.Settings, incoming.Config.Ports
		}
		combined.Config.Apps = append(append(target.Config.Apps[:0:0], target.Config.Apps...), incoming.Config.Apps...)
		combined.Apps = append(append(target.Apps[:0:0], target.Apps...), incoming.Apps...)
		combined.Ports.Assignments = append(append(target.Ports.Assignments[:0:0], target.Ports.Assignments...), incoming.Ports.Assignments...)
		combined.Files = make(map[string]File, len(target.Files)+len(incoming.Files))
		for name, record := range target.Files {
			combined.Files[name] = record
		}
		for name, record := range incoming.Files {
			previous, exists := combined.Files[name]
			if exists && previous.Directory != record.Directory {
				return fmt.Errorf("archives have conflicting entry types: %s", name)
			}
			if exists && previous != record && !newer {
				continue
			}
			if exists && previous != record && !record.Directory {
				replaced[name] = true
			}
			combined.Files[name] = record
		}
		if err := combined.Validate(); err != nil {
			return err
		}
	}
	// Sorted names create every parent directory before its entries.
	names := make([]string, 0, len(incoming.Files))
	for name := range incoming.Files {
		if _, exists := target.Files[name]; !exists || replaced[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		path := filepath.Join(destination, filepath.FromSlash(name))
		if incoming.Files[name].Directory {
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
			continue
		}
		if replaced[name] {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := moveFile(filepath.Join(source, filepath.FromSlash(name)), path); err != nil {
			return err
		}
	}
	*target = combined
	return nil
}

func moveFile(source, destination string) error {
	// Rename replaces silently; merged archive entries must never overlap.
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		if err == nil {
			err = fmt.Errorf("merged backup entry already exists: %s", destination)
		}
		return err
	}
	err := os.Rename(source, destination)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
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
	return errors.Join(copyErr, out.Close())
}
