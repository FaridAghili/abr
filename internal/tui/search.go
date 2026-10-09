package tui

import (
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// Letters belong to search; navigation uses arrows and control keys.
func listKeyMap() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Select.Up.SetKeys("up", "ctrl+k", "ctrl+p")
	k.Select.Down.SetKeys("down", "ctrl+j", "ctrl+n")
	k.Select.Left.SetKeys("left")
	k.Select.Right.SetKeys("right")
	k.Select.GotoTop.SetKeys("home")
	k.Select.GotoBottom.SetKeys("end")
	k.MultiSelect.Up.SetKeys("up", "ctrl+p")
	k.MultiSelect.Down.SetKeys("down", "ctrl+n")
	k.MultiSelect.GotoTop.SetKeys("home")
	k.MultiSelect.GotoBottom.SetKeys("end")
	k.MultiSelect.Toggle.SetKeys("space")
	return k
}

type searchableField interface {
	huh.Field
	GetFiltering() bool
}

func (m *model) listField() searchableField {
	if m.form != nil {
		field, _ := m.form.GetFocusedField().(searchableField)
		return field
	}
	return nil
}

func (m *model) clearSearch(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	field := m.listField()
	if field == nil {
		return nil, false
	}
	if !field.GetFiltering() && !key.Matches(msg, field.KeyBinds()...) {
		return nil, false
	}
	_, cmd := m.form.Update(msg)
	// huh first accepts an active filter on Esc, then clears it on Esc.
	// Clear it in one keystroke, leaving the current menu open.
	_, clear := m.form.Update(msg)
	return tea.Batch(cmd, clear), true
}

func (m *model) searchKey(msg tea.KeyPressMsg) tea.Cmd {
	field := m.listField()
	if field == nil {
		return nil
	}
	if msg.Code == tea.KeySpace && field.GetFiltering() {
		if _, ok := field.(*huh.MultiSelect[string]); ok {
			// Finish editing before toggling the highlighted match.
			_, cmd := m.form.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return cmd
		}
	}
	if field.GetFiltering() || msg.Mod & ^tea.ModShift != 0 || !unicode.IsPrint(msg.Code) || msg.Code == tea.KeySpace || msg.Code == '/' {
		return nil
	}
	_, cmd := m.form.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	return cmd
}

func (m *model) noSearchMatch() bool {
	if m.form == nil {
		return false
	}
	switch field := m.form.GetFocusedField().(type) {
	case *huh.Select[string]:
		_, ok := field.Hovered()
		return !ok
	case *huh.Select[bool]:
		_, ok := field.Hovered()
		return !ok
	case *huh.MultiSelect[string]:
		_, ok := field.Hovered()
		return !ok
	}
	return false
}
