package logview

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

type countedReader struct {
	io.ReaderAt
	bytes int
}

func (r *countedReader) ReadAt(p []byte, off int64) (int, error) {
	r.bytes += len(p)
	return r.ReaderAt.ReadAt(p, off)
}

func TestTailLineCountsAndBoundedReads(t *testing.T) {
	for _, tc := range []struct {
		content string
		lines   int
		want    string
	}{
		{"one\ntwo\nthree\n", 2, "two\nthree\n"},
		{"one\ntwo\nthree", 2, "two\nthree"},
		{"one\n", 10, "one\n"},
		{"", 10, ""},
		{"one\n\n", 1, "\n"},
	} {
		data, _, err := Tail(strings.NewReader(tc.content), int64(len(tc.content)), tc.lines, 4096)
		if err != nil || string(data) != tc.want {
			t.Fatalf("tail %q: %q, want %q (%v)", tc.content, data, tc.want, err)
		}
	}
	content := strings.Repeat("old log line\n", 100000) + "latest line\n"
	r := &countedReader{ReaderAt: strings.NewReader(content)}
	data, truncated, err := Tail(r, int64(len(content)), 1, 1024)
	if err != nil || !truncated || string(data) != "latest line\n" || r.bytes != 1024 {
		t.Fatal("tail read the whole file or missed the newest line")
	}
	unicode := []byte(strings.Repeat("✓", 100))
	data, truncated, err = Tail(bytes.NewReader(unicode), int64(len(unicode)), 100, 10)
	if err != nil || !truncated || !utf8.Valid(data) || len(data) > 10 {
		t.Fatal("bounded tail split a UTF-8 character")
	}
	// A file may be truncated between stat and the read; EOF is harmless.
	data, _, err = Tail(strings.NewReader(""), 100000, 100, 1024)
	if err != nil || len(data) != 0 {
		t.Fatal("concurrent truncation was not handled")
	}
}
