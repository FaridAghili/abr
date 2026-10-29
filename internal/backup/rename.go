package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"abr/internal/config"
)

// RenameApp gives the only app of an extracted archive a new name, so it can
// be restored beside an app with the same name. Its config, source record,
// port reservations, inventory and staged entries all move to the new name;
// host-specific fields such as the runtime user are left to the caller.
func RenameApp(m *Manifest, stage, name string) error {
	if len(m.Config.Apps) != 1 || len(m.Apps) != 1 {
		return fmt.Errorf("a new name restores one app; select a backup of a single app")
	}
	if !config.ValidName(name) {
		return fmt.Errorf("new name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens (max 63 characters)")
	}
	old := m.Config.Apps[0].Name
	if m.Apps[0].Name != old {
		return fmt.Errorf("backup app selection differs from config")
	}
	if name == old {
		return nil
	}
	moves := [][2]string{{"apps/" + old, "apps/" + name}, {"sql/" + old + ".sql", "sql/" + name + ".sql"}, {"state/databases/" + old + ".json", "state/databases/" + name + ".json"}, {"state/backups/" + old + ".json", "state/backups/" + name + ".json"}}
	renamed := func(entry string) string {
		for _, move := range moves {
			if entry == move[0] {
				return move[1]
			}
			if move[0] == "apps/"+old && strings.HasPrefix(entry, move[0]+"/") {
				return move[1] + strings.TrimPrefix(entry, move[0])
			}
		}
		return entry
	}
	files := make(map[string]File, len(m.Files))
	for entry, record := range m.Files {
		target := renamed(entry)
		if _, exists := files[target]; exists {
			return fmt.Errorf("renamed backup entry already exists: %s", target)
		}
		files[target] = record
	}
	// Check every destination before moving anything.
	var staged [][2]string
	for _, move := range moves {
		if _, ok := m.Files[move[0]]; !ok {
			continue
		}
		if _, err := os.Lstat(filepath.Join(stage, filepath.FromSlash(move[1]))); !os.IsNotExist(err) {
			if err == nil {
				err = fmt.Errorf("renamed backup entry already exists: %s", move[1])
			}
			return err
		}
		staged = append(staged, move)
	}
	for _, move := range staged {
		if err := os.Rename(filepath.Join(stage, filepath.FromSlash(move[0])), filepath.Join(stage, filepath.FromSlash(move[1]))); err != nil {
			return err
		}
	}
	m.Files = files
	m.Config.Apps[0].Name = name
	m.Apps[0].Name = name
	m.Ports.Assignments = slices.Clone(m.Ports.Assignments)
	for i := range m.Ports.Assignments {
		if m.Ports.Assignments[i].App == old {
			m.Ports.Assignments[i].App = name
		}
	}
	return m.Validate()
}

// ReplaceFile rewrites an extracted regular file and its inventory record.
func ReplaceFile(m *Manifest, stage, name string, data []byte) error {
	record, ok := m.Files[name]
	if !ok || record.Directory {
		return fmt.Errorf("backup file missing: %s", name)
	}
	path := filepath.Join(stage, filepath.FromSlash(name))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup entry must be a regular file: %s", name)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	record.Size, record.SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
	m.Files[name] = record
	return nil
}
