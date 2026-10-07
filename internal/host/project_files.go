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
	const limit = 1 << 20
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
		return nil, fmt.Errorf("project .env must be a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("project .env exceeds 1 MiB")
	}
	return data, nil
}
