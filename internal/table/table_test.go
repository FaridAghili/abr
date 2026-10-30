package table

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var fixture = Table{
	Title:   "Backups",
	Headers: []string{"APP", "SIZE"},
	Rows: [][]Cell{
		{Styled("shop", Strong), Text("12.0 MiB")},
		{Styled("blog\x1b]52;c;c2VjcmV0\a\ttwo", Bad), Text("1.0 KiB")},
	},
	Right: []int{1},
}

func TestWritePlainAlignsColumnsWithoutControls(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, fixture); err != nil {
		t.Fatal(err)
	}
	want := "Backups\nAPP       SIZE\nshop      12.0 MiB\nblog two  1.0 KiB\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

type renderer struct{ tables []Table }

func (r *renderer) Write(p []byte) (int, error) { return len(p), nil }
func (r *renderer) WriteTable(t Table) error    { r.tables = append(r.tables, t); return nil }

func TestWriteHandsTablesToRenderers(t *testing.T) {
	r := &renderer{}
	if err := Write(r, fixture); err != nil || len(r.tables) != 1 || r.tables[0].Title != "Backups" {
		t.Fatal("table not handed to renderer", err, r.tables)
	}
}

func TestRenderFitsWidthAndStripsControls(t *testing.T) {
	out := Render(fixture, Colors(true), 80)
	plain := ansi.Strip(out)
	if strings.Contains(out, "]52;") || strings.Contains(out, "\a") {
		t.Fatal("terminal control from a cell was rendered", out)
	}
	if !strings.Contains(plain, "╭") || !strings.Contains(plain, "│ shop ") || !strings.Contains(plain, "12.0 MiB │") {
		t.Fatal("table not drawn with borders and right-aligned sizes", plain)
	}
	long := fixture
	long.Rows = append(long.Rows, []Cell{Text(strings.Repeat("very-long-app-name ", 8)), Text("1 B")})
	for _, line := range strings.Split(Render(long, Colors(false), 40), "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("line wider than 40 columns: %q", ansi.Strip(line))
		}
	}
}
