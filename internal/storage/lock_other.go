//go:build !darwin && !linux

package storage

import (
	"fmt"
	"os"
)

func lock(*os.File) error { return fmt.Errorf("file locking is supported only on macOS and Linux") }
func unlock(*os.File)     {}
