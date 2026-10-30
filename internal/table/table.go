// Package table presents lists and records as aligned, colored tables in
// terminals and as plain tab-aligned text for scripts.
package table

import (
	"image/color"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"charm.land/lipgloss/v2"
	lipglosstable "charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// Tone gives a cell's meaning; terminals show it as a color.
type Tone int

const (
	Plain  Tone = iota
	Strong      // The row's name or a total.
	Muted       // Absent or not applicable.
	Good
	Warn
	Bad
)

type Cell struct {
	Text string
	Tone Tone
}

func Text(s string) Cell              { return Cell{Text: s} }
func Styled(s string, tone Tone) Cell { return Cell{Text: s, Tone: tone} }
func Texts(values ...string) (row []Cell) {
	for _, v := range values {
		row = append(row, Text(v))
	}
	return row
}

type Table struct {
	Title   string // Optional caption above the table.
	Headers []string
	Rows    [][]Cell
	Right   []int // Columns aligned right, such as sizes and counts.
}

// Renderer is implemented by outputs that draw tables themselves, such as the TUI.
type Renderer interface{ WriteTable(Table) error }

// Write draws t for w: through w's own Renderer, as a colored table on an
// interactive terminal, or as plain tab-aligned text otherwise.
func Write(w io.Writer, t Table) error {
	if r, ok := w.(Renderer); ok {
		return r.WriteTable(t)
	}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		width, _, err := term.GetSize(int(f.Fd()))
		if err != nil || width <= 0 {
			width = 100
		}
		// Fprintln downsamples colors for the terminal and honors NO_COLOR.
		_, err = lipgloss.Fprintln(w, Render(t, ANSITheme(), width))
		return err
	}
	return WritePlain(w, t)
}

// WritePlain keeps one row per line with tab-aligned columns for scripts.
func WritePlain(w io.Writer, t Table) error {
	if t.Title != "" {
		if _, err := io.WriteString(w, oneLine(t.Title)+"\n"); err != nil {
			return err
		}
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if len(t.Headers) > 0 {
		if _, err := io.WriteString(tw, strings.Join(t.Headers, "\t")+"\n"); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		values := make([]string, len(row))
		for i, cell := range row {
			values[i] = oneLine(cell.Text)
		}
		if _, err := io.WriteString(tw, strings.Join(values, "\t")+"\n"); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// Theme styles each part of a rendered table.
type Theme struct {
	Title, Header, Border, Plain, Strong, Muted, Good, Warn, Bad lipgloss.Style
}

func (th Theme) tone(t Tone) lipgloss.Style {
	switch t {
	case Strong:
		return th.Strong
	case Muted:
		return th.Muted
	case Good:
		return th.Good
	case Warn:
		return th.Warn
	case Bad:
		return th.Bad
	}
	return th.Plain
}

// Colors matches the TUI palette for a dark or light background.
func Colors(dark bool) Theme {
	choose := lipgloss.LightDark(dark)
	fg := func(light, dark string) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(choose(lipgloss.Color(light), lipgloss.Color(dark)))
	}
	return Theme{
		Title:  fg("#5145B5", "#8B9FFF").Bold(true),
		Header: fg("#A3266F", "#F5A3D6").Bold(true),
		Border: fg("#8C959F", "#4A5260"),
		Plain:  fg("#25313D", "#E2E8F0"),
		Strong: fg("#0B6E99", "#7DD3FC").Bold(true),
		Muted:  fg("#59636F", "#9198A1"),
		Good:   fg("#187346", "#68D391"),
		Warn:   fg("#9A5B00", "#F6C177"),
		Bad:    fg("#B4233C", "#FF838B").Bold(true),
	}
}

// ANSITheme uses the terminal's own palette, which suits light and dark themes.
func ANSITheme() Theme {
	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	return Theme{
		Title:  fg(lipgloss.Blue).Bold(true),
		Header: fg(lipgloss.Magenta).Bold(true),
		Border: fg(lipgloss.BrightBlack),
		Plain:  lipgloss.NewStyle(),
		Strong: fg(lipgloss.Cyan).Bold(true),
		Muted:  fg(lipgloss.BrightBlack),
		Good:   fg(lipgloss.Green),
		Warn:   fg(lipgloss.Yellow),
		Bad:    fg(lipgloss.Red).Bold(true),
	}
}

// Render draws t with rounded borders, at its natural width up to width
// columns; wider tables wrap their longest cells.
func Render(t Table, th Theme, width int) string {
	right := map[int]bool{}
	for _, i := range t.Right {
		right[i] = true
	}
	rows := make([][]string, len(t.Rows))
	for i, row := range t.Rows {
		rows[i] = make([]string, len(row))
		for j, cell := range row {
			rows[i][j] = sanitize(cell.Text)
		}
	}
	build := func() *lipglosstable.Table {
		return lipglosstable.New().Border(lipgloss.RoundedBorder()).BorderStyle(th.Border).
			Headers(t.Headers...).Rows(rows...).
			StyleFunc(func(row, col int) lipgloss.Style {
				style := th.Plain
				if row == lipglosstable.HeaderRow {
					style = th.Header
				} else if row < len(t.Rows) && col < len(t.Rows[row]) {
					style = th.tone(t.Rows[row][col].Tone)
				}
				style = style.Padding(0, 1)
				if right[col] && row != lipglosstable.HeaderRow {
					style = style.Align(lipgloss.Right)
				}
				return style
			})
	}
	out := build().String()
	if width > 0 && lipgloss.Width(out) > width {
		out = build().Width(width).Wrap(true).String()
	}
	if t.Title != "" {
		out = th.Title.Render(ansi.Truncate(sanitize(t.Title), max(1, width), "…")) + "\n" + out
	}
	return out
}

// sanitize removes terminal controls from untrusted cell text.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(sanitize(s), "\n", " ")), " ")
}
