//go:build darwin || linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lock(f *os.File) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 10s waiting for another sites process")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
