package project

import (
	"fmt"
	"os"
	"path/filepath"
)

// HasEnvExample checks whether environment preparation is part of this project.
// A missing example is optional; unsafe or unreadable paths are still errors.
func HasEnvExample(directory string) (bool, error) {
	info, err := os.Lstat(filepath.Join(directory, ".env.example"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf(".env.example must be a regular file without symlinks")
	}
	return true, nil
}
