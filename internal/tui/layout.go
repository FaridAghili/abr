package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func (m *model) footer() string {
	switch m.page {
	case "details":
		return "↑/↓ scroll · Enter / Esc back"
	case "confirm":
		return "←/→ choose · Enter confirm · Esc back"
	case "output":
		footer := "↑/↓ scroll"
		if !m.busy && isLogView(m.current.args) {
			footer += " · r refresh"
		}
		if !m.busy {
			if m.current.continueWith != nil && m.result == nil && !m.options.DryRun {
				footer += " · Enter continue · Esc cancel"
			} else {
				footer += " · Enter / Esc back"
			}
		}
		if len(m.current.args) > 0 && (m.current.args[0] == "logs" || m.current.args[0] == "doctor") {
			footer += "\n" + ansi.Truncate(m.current.note, m.bodyWidth(), "…")
		}
		return footer
	}
	footer := "↑/↓ select · Enter open · Esc back"
	if m.page == "form" {
		footer = "Enter next · Shift+Tab back · Esc cancel"
		if m.form != nil {
			switch m.form.GetFocusedField().(type) {
			case *huh.MultiSelect[string]:
				footer = "Space toggle · Enter next\nShift+Tab back · Esc cancel"
			case *huh.Select[string], *huh.Select[bool]:
				footer = "↑/↓ choose · Enter next\nShift+Tab back · Esc cancel"
			}
		}
	}
	if m.listField() != nil {
		footer += "\nType to filter · Esc clears search"
	}
	return footer
}

func (m *model) contentHeight() int {
	// Four header rows and two padding rows; controls stay below the body.
	return max(1, m.height-6-lipgloss.Height(m.footer()))
}

func (m *model) bodyHeight() int {
	h := m.contentHeight()
	if m.notice != "" {
		h--
	}
	if m.context != "" && m.page == "app" {
		h -= lipgloss.Height(m.context) + 1
	}
	return max(1, h)
}

func (m *model) formHeight() int { return m.bodyHeight() }

func (m *model) resizeForm() tea.Cmd {
	if m.form == nil {
		return nil
	}
	m.form.WithWidth(m.bodyWidth()).WithHeight(m.formHeight())
	field := m.form.GetFocusedField()
	h := m.formHeight()
	if err := field.Error(); err != nil {
		h -= lipgloss.Height(ansi.Wrap(clean(err.Error()), m.bodyWidth(), "")) + 1
	}
	// huh shrinks fields automatically, but does not grow them on resize.
	// Each step has one field, so give it all the available body rows.
	field.WithHeight(max(1, h))
	_, cmd := m.form.Update(tea.WindowSizeMsg{Width: m.bodyWidth(), Height: m.formHeight()})
	return cmd
}

func (m *model) resizeLayout() tea.Cmd {
	m.viewport.SetWidth(m.bodyWidth())
	h := m.bodyHeight()
	switch m.page {
	case "confirm":
		h -= 2
	case "output":
		h--
	}
	m.viewport.SetHeight(max(1, h))
	return m.resizeForm()
}
