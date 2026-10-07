package host

import "abr/internal/config"

func (h Host) Edit(name string, changes config.AppChanges) error {
	return h.locked(func() error {
		if err := h.Manager.Edit(name, changes, !h.DryRun); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Settings preview validated for %s; no files or services changed", name)
		} else {
			h.say("Saved settings for %s; run abr deploy %s to apply them", name, name)
		}
		return nil
	})
}
