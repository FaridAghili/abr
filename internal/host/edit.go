package host

import (
	"slices"

	"abr/internal/config"
)

func (h Host) Edit(name string, changes config.AppChanges) error {
	return h.locked(func() error {
		if err := h.Manager.Edit(name, changes, !h.DryRun); err != nil {
			return err
		}
		backupOnly := true
		for _, field := range changes.Fields {
			backupOnly = backupOnly && (field == "backup-schedule" || field == "backup-keep")
		}
		if slices.Contains(changes.Fields, "backup-schedule") {
			if err := h.ensureBackupTimer(); err != nil {
				return err
			}
		}
		switch {
		case h.DryRun:
			h.say("Settings preview validated for %s; no files or services changed", name)
		case backupOnly:
			h.say("Saved backup settings for %s; abr-backup.timer checks hourly and backs it up when due", name)
		default:
			h.say("Saved settings for %s; run abr deploy %s to apply them", name, name)
		}
		return nil
	})
}
