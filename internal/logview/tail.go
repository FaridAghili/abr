// Package logview reads bounded log tails without scanning entire files.
package logview

import (
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

func Tail(r io.ReaderAt, size int64, lines, byteLimit int) ([]byte, bool, error) {
	if size <= 0 || lines <= 0 || byteLimit <= 0 {
		return nil, false, nil
	}
	start := max(int64(0), size-int64(byteLimit))
	data := make([]byte, size-start)
	n, err := r.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	data = data[:n]
	if start > 0 {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
	}
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	position := end
	for range lines {
		newline := bytes.LastIndexByte(data[:position], '\n')
		if newline < 0 {
			return data, start > 0, nil
		}
		position = newline
	}
	return data[position+1:], true, nil
}
