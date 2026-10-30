package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Menu options that list apps are drawn as aligned, colored columns. An
// option's key stays plain text, so typing still filters by it; View swaps in
// the colored cells registered for that text when the form is drawn.

type tone int

const (
	toneText tone = iota
	toneName
	toneMuted
	toneGood
	toneInfo
)

type tableCell struct {
	text string
	tone tone
}

func cellText(s string) tableCell         { return tableCell{text: clean(s)} }
func cellTone(s string, t tone) tableCell { return tableCell{text: clean(s), tone: t} }

// tableOptions aligns rows under header for options indented by indent
// columns. It returns the header line and each row's plain key, and registers
// their colors for the next form. A too-wide last column is truncated.
func (m *model) tableOptions(indent int, header []string, rows [][]tableCell) (string, []string) {
	const gap = 2
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], lipgloss.Width(c.text))
		}
	}
	// The field's left border and padding take two columns.
	if last := len(widths) - 1; last >= 0 {
		used := indent + 2
		for _, w := range widths[:last] {
			used += w + gap
		}
		widths[last] = max(1, min(widths[last], m.bodyWidth()-used))
	}
	line := func(cells []tableCell) (string, []tableCell) {
		padded := make([]tableCell, len(cells))
		var plain strings.Builder
		for i, c := range cells {
			text := ansi.Truncate(c.text, widths[i], "…")
			if i < len(cells)-1 {
				text += strings.Repeat(" ", widths[i]-lipgloss.Width(text)+gap)
			}
			padded[i] = tableCell{text: text, tone: c.tone}
			plain.WriteString(text)
		}
		return strings.TrimRight(plain.String(), " "), padded
	}
	if m.nextRows == nil {
		m.nextRows = map[string][]tableCell{}
	}
	headCells := make([]tableCell, len(header))
	for i, h := range header {
		headCells[i] = tableCell{text: h, tone: -1}
	}
	head, cells := line(headCells)
	head = strings.Repeat(" ", indent) + head
	m.nextRows[head] = append([]tableCell{{text: strings.Repeat(" ", indent), tone: -1}}, cells...)
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i], cells = line(row)
		m.nextRows[keys[i]] = cells
	}
	return head, keys
}

// selectIndent and multiSelectIndent are the columns before an option's text.
func (m *model) selectIndent() int {
	return lipgloss.Width(formTheme(m.dark).Focused.SelectSelector.String())
}

func (m *model) multiSelectIndent() int {
	t := formTheme(m.dark).Focused
	return lipgloss.Width(t.MultiSelectSelector.String()) + lipgloss.Width(t.UnselectedPrefix.String())
}

// colorRows replaces registered plain rows in a drawn form with their colors;
// the row under the cursor shows its name in the selection color.
func (m *model) colorRows(view string) string {
	if len(m.rows) == 0 {
		return view
	}
	p := colors(m.dark)
	styles := map[tone]lipgloss.Style{toneText: p.text, toneName: p.text.Bold(true), toneMuted: p.muted, toneGood: p.success.UnsetBold(), toneInfo: p.info, -1: p.accent}
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		plain := ansi.Strip(line)
		for key, cells := range m.rows {
			at := strings.Index(plain, key)
			if at < 0 {
				continue
			}
			cursor := strings.Contains(plain[:at], "▸")
			var b strings.Builder
			for _, c := range cells {
				style := styles[c.tone]
				if cursor && c.tone == toneName {
					style = p.key
				}
				b.WriteString(style.Render(c.text))
			}
			col, width := ansi.StringWidth(plain[:at]), ansi.StringWidth(key)
			lines[i] = ansi.Cut(line, 0, col) + b.String() + ansi.Cut(line, col+width, ansi.StringWidth(line))
			break
		}
	}
	return strings.Join(lines, "\n")
}
