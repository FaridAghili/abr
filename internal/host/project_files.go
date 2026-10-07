package host

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// App-owned files may change while the root manager is reading them. Never
// follow a replacement symlink, block on a FIFO, or allocate without a limit.
func readProjectEnv(path string) ([]byte, error) {
	return readProjectFile(path, 1<<20)
}

func readProjectFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("project file must be a regular file of at most %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("project file exceeds %d bytes", limit)
	}
	return data, nil
}
