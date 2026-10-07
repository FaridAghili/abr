package host

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sites-manager/internal/config"
)

type transferRunner struct{ run func(Command) ([]byte, error) }

func (r transferRunner) Run(c Command) ([]byte, error) { return r.run(c) }

func transferFixture(t *testing.T) (Host, credentials) {
	t.Helper()
	h, _, _, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	c, err := h.transferCredentials(a)
	if err != nil {
		t.Fatal(err)
	}
	return h, c
}

func TestBackupSelectionAndPrivateAtomicFiles(t *testing.T) {
	h, c := transferFixture(t)
	directory := filepath.Join(h.Manager.StateDir, "backups")
	dump := "-- SQL data, not terminal output\n"
	calls := 0
	h.Runner = transferRunner{func(cmd Command) ([]byte, error) {
		calls++
		if cmd.Name != "mysqldump" || !cmd.Private || cmd.Stdout == nil || strings.Contains(strings.Join(cmd.Args, " "), c.Password) {
			t.Fatalf("unsafe dump: %+v", cmd)
		}
		args := strings.Join(cmd.Args, " ")
		for _, flag := range []string{"--single-transaction", "--quick", "--set-gtid-purged=OFF", "--routines", "--events", "--triggers", c.Database} {
			if !strings.Contains(args, flag) {
				t.Fatal(args)
			}
		}
		_, err := io.WriteString(cmd.Stdout, dump)
		return nil, err
	}}
	for _, names := range [][]string{{}, {"app", "app"}, {"unknown"}, {"app", "unknown"}} {
		if err := h.BackupDatabases(names, false, directory); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid selection executed a dump")
	}
	if err := h.BackupDatabases(nil, true, directory); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "app-") || !strings.HasSuffix(entries[0].Name(), ".sql") {
		t.Fatal(entries)
	}
	path := filepath.Join(directory, entries[0].Name())
	info, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0600 || string(data) != dump {
		t.Fatal("backup not private or complete")
	}
	if strings.Contains(h.Output.(*bytes.Buffer).String(), dump) {
		t.Fatal("SQL leaked to output")
	}
	if err := h.dumpDatabase(c, path); err == nil {
		t.Fatal("overwrote existing backup")
	}
	h.Runner = transferRunner{func(cmd Command) ([]byte, error) {
		io.WriteString(cmd.Stdout, "partial")
		return nil, errors.New("dump failed")
	}}
	if err := h.BackupDatabases([]string{"app"}, false, directory); err == nil {
		t.Fatal("failed dump reported success")
	}
	entries, _ = os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatal("partial file left behind", entries)
	}
}

func TestBackupAllFiltersAndRejectsUnmanagedDatabase(t *testing.T) {
	h, _ := transferFixture(t)
	a := config.App{Name: "web", Directory: "/srv/apps/web", User: "sites-web", Type: "nuxt", Domain: "web.test"}
	if _, err := h.Manager.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	h.DryRun = true
	if err := h.BackupDatabases(nil, true, filepath.Join(h.Manager.StateDir, "backups")); err != nil {
		t.Fatal(err)
	}
	if err := h.BackupDatabases([]string{"web"}, false, "backups"); err == nil {
		t.Fatal("unmanaged database selected")
	}
}

func TestImportIsScopedPrivateStreamingAndConfirmed(t *testing.T) {
	h, c := transferFixture(t)
	path := filepath.Join(h.Manager.StateDir, "input.sql")
	sql := "CREATE TABLE demo(id INT);\nINSERT INTO demo VALUES(1);\n"
	os.WriteFile(path, []byte(sql), 0600)
	called := false
	var defaults string
	h.Runner = transferRunner{func(cmd Command) ([]byte, error) {
		called = true
		if cmd.Name != "mysql" || !cmd.Private || cmd.Stdin == nil || cmd.Stdout == nil {
			t.Fatal("unsafe import")
		}
		args := strings.Join(cmd.Args, " ")
		if strings.Contains(args, c.Password) || strings.Contains(args, "--user=root") || !strings.Contains(args, "--binary-mode=1") || !strings.Contains(args, "--local-infile=0") || !strings.Contains(args, "--database="+c.Database) {
			t.Fatal("unscoped import", args)
		}
		defaults = strings.TrimPrefix(cmd.Args[0], "--defaults-file=")
		info, _ := os.Stat(defaults)
		data, _ := os.ReadFile(defaults)
		if info.Mode().Perm() != 0600 || !strings.Contains(string(data), "user="+c.User) || !strings.Contains(string(data), c.Password) {
			t.Fatal("unsafe credentials file")
		}
		input, _ := io.ReadAll(cmd.Stdin)
		if string(input) != sql {
			t.Fatal("SQL not streamed")
		}
		return nil, nil
	}}
	if err := h.ImportDatabase("app", path, false); err == nil || called {
		t.Fatal("unconfirmed import executed")
	}
	for _, invalid := range []string{h.Manager.StateDir, filepath.Join(h.Manager.StateDir, "missing.sql")} {
		if err := h.ImportDatabase("app", invalid, true); err == nil || called {
			t.Fatal("invalid file imported")
		}
	}
	link := filepath.Join(h.Manager.StateDir, "link.sql")
	os.Symlink(path, link)
	if err := h.ImportDatabase("app", link, true); err == nil || called {
		t.Fatal("symlink imported")
	}
	if err := h.ImportDatabase("app", path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaults); !os.IsNotExist(err) {
		t.Fatal("temporary password persisted")
	}
	if strings.Contains(h.Output.(*bytes.Buffer).String(), sql) || strings.Contains(h.Output.(*bytes.Buffer).String(), c.Password) {
		t.Fatal("private data leaked")
	}
	h.Runner = transferRunner{func(Command) ([]byte, error) { return nil, errors.New("failed") }}
	if err := h.ImportDatabase("app", path, true); err == nil || !strings.Contains(err.Error(), "partially") {
		t.Fatal("failure not reported", err)
	}
	files, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, ".sites-mysql-*"))
	if len(files) != 0 {
		t.Fatal("credentials not cleaned after failure")
	}
}

func TestExecRunnerStreamsWithoutLoggingOrBuffering(t *testing.T) {
	var output, dump bytes.Buffer
	runner := ExecRunner{Output: &output}
	result, err := runner.Run(Command{Name: "cat", Stdin: strings.NewReader("private SQL"), Stdout: &dump, Private: true})
	if err != nil || len(result) != 0 || output.Len() != 0 || dump.String() != "private SQL" {
		t.Fatal(result, err, output.String(), dump.String())
	}
}
