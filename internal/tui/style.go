package tui

import (
	"image/color"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type palette struct {
	accent, muted, danger, success, warning, info, key, text, badge, preview lipgloss.Style
	rule                                                                     string
	accentColor, trackColor                                                  color.Color
}

func colors(dark bool) palette {
	choose := lipgloss.LightDark(dark)
	shade := func(light, dark string) color.Color { return choose(lipgloss.Color(light), lipgloss.Color(dark)) }
	style := func(light, dark string) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(choose(lipgloss.Color(light), lipgloss.Color(dark)))
	}
	return palette{
		accent:      style("#5145B5", "#8B9FFF").Bold(true),
		muted:       style("#59636F", "#9198A1"),
		danger:      style("#B4233C", "#FF838B").Bold(true),
		success:     style("#187346", "#68D391").Bold(true),
		warning:     style("#9A5B00", "#F6C177").Bold(true),
		info:        style("#0B6E99", "#7DD3FC"),
		key:         style("#A3266F", "#F5A3D6").Bold(true),
		text:        style("#25313D", "#E2E8F0"),
		badge:       lipgloss.NewStyle().Bold(true).Padding(0, 1).Foreground(lipgloss.Color("#FFFFFF")).Background(choose(lipgloss.Color("#5145B5"), lipgloss.Color("#5B5BD6"))),
		preview:     lipgloss.NewStyle().Bold(true).Padding(0, 1).Foreground(lipgloss.Color("#1F1300")).Background(choose(lipgloss.Color("#E8A33D"), lipgloss.Color("#F6C177"))),
		rule:        "─",
		accentColor: shade("#5145B5", "#8B9FFF"),
		trackColor:  shade("#D0D7DE", "#3A4250"),
	}
}

func formTheme(dark bool) *huh.Styles {
	p := colors(dark)
	theme := huh.ThemeCharm(dark)
	choose := lipgloss.LightDark(dark)
	pink := choose(lipgloss.Color("#A3266F"), lipgloss.Color("#F5A3D6"))
	// The stock theme uses dark text for unselected options on dark terminals.
	foreground := choose(lipgloss.Color("#25313D"), lipgloss.Color("#E2E8F0"))
	for _, field := range []*huh.FieldStyles{&theme.Focused, &theme.Blurred} {
		field.Title = p.accent
		field.Description = p.muted
		field.Option = field.Option.Foreground(foreground)
		field.UnselectedOption = field.UnselectedOption.Foreground(foreground)
		field.SelectedOption = lipgloss.NewStyle().Foreground(pink).Bold(true)
		field.SelectSelector = lipgloss.NewStyle().Foreground(pink).Bold(true).SetString("▸ ")
		field.MultiSelectSelector = field.SelectSelector
		field.TextInput.Prompt = lipgloss.NewStyle().Foreground(pink).Bold(true)
		field.TextInput.Cursor = lipgloss.NewStyle().Foreground(pink)
	}
	theme.Focused.Base = theme.Focused.Base.BorderForeground(p.accentColor)
	theme.Group.Title, theme.Group.Description = p.accent, p.muted
	return theme
}

// Keys in control hints are highlighted; the plain text stays identical.
var hintKeys = regexp.MustCompile(`↑/↓|←/→|Shift\+Tab|Enter|Esc|Space|Tab|\b[rs]\b|Type`)

func (p palette) hints(footer string) string {
	lines := strings.Split(footer, "\n")
	for i, line := range lines {
		var b strings.Builder
		last := 0
		for _, match := range hintKeys.FindAllStringIndex(line, -1) {
			b.WriteString(p.hintText(line[last:match[0]]))
			b.WriteString(p.key.Render(line[match[0]:match[1]]))
			last = match[1]
		}
		b.WriteString(p.hintText(line[last:]))
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

func (p palette) hintText(s string) string {
	if s == "" {
		return ""
	}
	return p.muted.Render(s)
}

// bar renders a filled gauge; color reflects how heavy the share is.
func (p palette) bar(percent float64, width int, light, heavy float64) string {
	width = max(1, width)
	filled := int(percent/100*float64(width) + 0.5)
	if percent > 0 && filled == 0 {
		filled = 1
	}
	filled = min(filled, width)
	level := p.level(percent, light, heavy)
	return level.UnsetBold().Render(strings.Repeat("█", filled)) + lipgloss.NewStyle().Foreground(p.trackColor).Render(strings.Repeat("░", width-filled))
}

func (p palette) level(percent, light, heavy float64) lipgloss.Style {
	switch {
	case percent >= heavy:
		return p.danger
	case percent >= light:
		return p.warning
	}
	return p.success
}

func (p palette) title(title string, width int) string {
	return p.accent.Render("▍"+ansi.Truncate(title, width-1, "…")) + "\n" + p.muted.Render(strings.Repeat(p.rule, width))
}
