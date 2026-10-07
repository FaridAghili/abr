package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLockAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("ABR_LOCK_TEST_DIR"); dir != "" {
		for i := 0; i < 3; i++ {
			err := WithLock(filepath.Join(dir, "counter.lock"), func() error {
				data, err := os.ReadFile(filepath.Join(dir, "counter"))
				if err != nil {
					return err
				}
				n, err := strconv.Atoi(string(data))
				if err != nil {
					return err
				}
				time.Sleep(10 * time.Millisecond) // Deliberately widen the lost-update window.
				return AtomicWrite(filepath.Join(dir, "counter"), []byte(strconv.Itoa(n+1)))
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "counter"), []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockAcrossProcesses$")
			cmd.Env = append(os.Environ(), "ABR_LOCK_TEST_DIR="+dir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				results <- fmt.Errorf("subprocess: %w: %s", err, output)
			}
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		t.Error(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "counter"))
	if err != nil || string(data) != "18" {
		t.Fatalf("lost update: %s (%v)", data, err)
	}
}

func TestAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state")
	if err := AtomicWrite(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("%s %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	// Rename onto a directory must fail and clean up the temporary file.
	dir := filepath.Dir(path)
	if err := AtomicWrite(dir, []byte("cannot replace directory")); err == nil {
		t.Fatal("expected rename failure")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dir), ".abr-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temp files left: %v %v", matches, err)
	}
}
