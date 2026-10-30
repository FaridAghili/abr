package host

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// ScheduleBackups gives every selected app the same schedule and, when keep is
// at least 0, the same retention, in one configuration change.
func (h Host) ScheduleBackups(names []string, all bool, schedule string, keep int) error {
	if all == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
	}
	changes := config.AppChanges{Values: config.App{Backup: config.Backup{Schedule: schedule, Keep: keep}}, Fields: []string{"backup-schedule"}}
	if keep >= 0 {
		changes.Fields = append(changes.Fields, "backup-keep")
	}
	if err := h.locked(func() error {
		c, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		selected, err := selectBackupApps(c, names, all)
		if err != nil {
			return err
		}
		names = names[:0]
		for _, a := range selected {
			names = append(names, a.Name)
		}
		if err := h.Manager.EditApps(names, changes, !h.DryRun); err != nil {
			return err
		}
		if schedule != "off" {
			if err := h.ensureBackupTimer(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	plan := "manual only"
	if schedule != "off" {
		plan = map[string]string{"daily": "daily", "weekly": "weekly"}[schedule]
		if plan == "" {
			plan = "every " + schedule
		}
	}
	switch {
	case keep == 0:
		plan += ", keep all"
	case keep > 0:
		plan += ", keep newest " + strconv.Itoa(keep)
	}
	if h.DryRun {
		h.say("Would set %s for %d apps: %s; no files changed", plan, len(names), strings.Join(names, ", "))
		return nil
	}
	h.say("Saved %s for %d apps: %s", plan, len(names), strings.Join(names, ", "))
	return h.BackupStatus()
}
