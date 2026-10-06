// Package storage provides local file locking and atomic replacement on macOS and Linux.
package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// WithLock locks a separate, permanent file: locking a replaced data file is unsafe.
// All writers must use the same path. The lock is released on process exit too.
func WithLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", path, err)
	}
	defer f.Close()
	if err := lock(f); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer unlock(f)
	return fn()
}

// AtomicWrite never truncates the old file. Temp files stay on the same filesystem.
func AtomicWrite(path string, data []byte) error {
	return AtomicWriteMode(path, data, 0600)
}

// AtomicWriteMode sets permissions before exposing the replacement file.
func AtomicWriteMode(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sites-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
