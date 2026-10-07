package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"abr/internal/config"
)

// Transfers use only existing, recorded managed databases; they never provision one.
func (h Host) transferCredentials(a config.App) (credentials, error) {
	var c credentials
	if !a.Database.Enabled {
		return c, fmt.Errorf("%s: managed database is disabled", a.Name)
	}
	data, err := h.read(h.credentialsPath(a.Name))
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("corrupt database credentials: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || !c.Ready || c.App != a.Name || c.Database != databaseName(a.Name) || c.User != RuntimeUser(a.Name) || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
		return c, fmt.Errorf("invalid or incomplete database ownership record for %s", a.Name)
	}
	return c, nil
}

func (h Host) BackupDatabases(names []string, all bool, directory string) error {
	if all == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
	}
	if directory == "" {
		return fmt.Errorf("--output-dir is required")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	return h.locked(func() error {
		cfg, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		if all {
			for _, a := range cfg.Apps {
				if a.Database.Enabled {
					names = append(names, a.Name)
				}
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("no managed databases selected")
		}
		seen := map[string]bool{}
		records := make([]credentials, 0, len(names))
		// Validate the entire selection before writing any dumps.
		for _, name := range names {
			if seen[name] {
				return fmt.Errorf("duplicate database selection %s", name)
			}
			seen[name] = true
			index := slices.IndexFunc(cfg.Apps, func(a config.App) bool { return a.Name == name })
			if index < 0 {
				return fmt.Errorf("unknown application %q", name)
			}
			if !cfg.Apps[index].Database.Enabled {
				return fmt.Errorf("%s: managed database is disabled", name)
			}
			c := credentials{App: name, Database: databaseName(name)}
			if !h.DryRun {
				c, err = h.transferCredentials(cfg.Apps[index])
				if err != nil {
					return err
				}
			}
			records = append(records, c)
		}
		if h.DryRun {
			for _, c := range records {
				h.say("Would export %s into a private SQL file under %s", c.Database, directory)
			}
			return nil
		}
		// A private directory avoids inherited ACLs exposing database data.
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return err
		}
		if resolved != directory {
			return fmt.Errorf("backup directory must not contain symlinks")
		}
		if err := h.trustedDirectory(directory); err != nil {
			return err
		}
		info, err := os.Stat(directory)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm() != 0700 {
			return fmt.Errorf("backup directory must be owned by root with mode 0700; use a new private directory")
		}
		var failures []error
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		for _, c := range records {
			path := filepath.Join(directory, c.App+"-"+stamp+".sql")
			if err := h.dumpDatabase(c, path); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", c.App, err))
				continue
			}
			h.say("Database backup: %s", path)
		}
		return errors.Join(failures...)
	})
}

func (h Host) dumpDatabase(c credentials, path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".abr-dump-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	_, err = h.run("Export managed database "+c.Database, Command{Name: "mysqldump", Args: []string{
		"--protocol=socket", "--user=root", "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--no-tablespaces", "--set-gtid-purged=OFF", "--hex-blob", "--default-character-set=utf8mb4", c.Database,
	}, Stdout: f, Private: true})
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Link publishes the complete file atomically and refuses to replace any existing file.
	if err := os.Link(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (h Host) ImportDatabase(name, path string, yes bool) error {
	if !yes && !h.DryRun {
		return fmt.Errorf("import may overwrite database data; back up first and pass --yes")
	}
	if strings.ToLower(filepath.Ext(path)) != ".sql" {
		return fmt.Errorf("import requires a .sql file")
	}
	return h.locked(func() error {
		a, _, err := h.application(name)
		if err != nil {
			return err
		}
		if !a.Database.Enabled {
			return fmt.Errorf("%s: managed database is disabled", name)
		}
		if h.DryRun {
			h.say("Would import %s into the managed database for %s using its scoped MySQL account; data may be overwritten", path, name)
			return nil
		}
		c, err := h.transferCredentials(a)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("SQL import must be a nonempty regular file")
		}
		// Private temporary defaults file keeps passwords out of process arguments/environment.
		defaults, err := os.CreateTemp(h.Manager.StateDir, ".abr-mysql-*")
		if err != nil {
			return err
		}
		defer os.Remove(defaults.Name())
		defer defaults.Close()
		text := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nprotocol=socket\n", c.User, c.Password)
		if _, err := io.WriteString(defaults, text); err != nil {
			return err
		}
		if err := defaults.Close(); err != nil {
			return err
		}
		_, err = h.run("Import SQL into "+c.Database+" with its scoped account", Command{Name: "mysql", Args: []string{
			"--defaults-file=" + defaults.Name(), "--no-login-paths", "--binary-mode=1", "--local-infile=0", "--default-character-set=utf8mb4", "--database=" + c.Database,
		}, Stdin: f, Stdout: io.Discard, Private: true})
		if err != nil {
			return fmt.Errorf("import failed; database may be partially changed: %w", err)
		}
		h.say("Imported SQL into %s", c.Database)
		return nil
	})
}
