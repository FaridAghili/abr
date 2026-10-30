package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"abr/internal/config"
	"abr/internal/table"
)

// A timer tick may start a little before an interval elapses; this slack keeps
// an "every N hours" schedule on its hourly tick instead of drifting by one.
const backupSlack = 10 * time.Minute

// backupRecord is the last scheduled-backup outcome of one app.
type backupRecord struct {
	LastSuccess time.Time `json:",omitzero"`
	LastAttempt time.Time
	LastError   string `json:",omitempty"`
}

func (h Host) backupRecordPath(name string) string {
	return filepath.Join(h.Manager.StateDir, "backups", name+".json")
}

func (h Host) loadBackupRecord(name string) (backupRecord, error) {
	var record backupRecord
	data, err := h.read(h.backupRecordPath(name))
	if os.IsNotExist(err) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("corrupt backup record for %s: %w", name, err)
	}
	return record, nil
}

// recordBackup saves an attempt. A failure keeps the last success, so the app
// stays due and the next hourly run retries it.
func (h Host) recordBackup(name string, started time.Time, failure error) error {
	record, err := h.loadBackupRecord(name)
	if err != nil {
		return err
	}
	record.LastAttempt, record.LastError = started, ""
	if failure == nil {
		record.LastSuccess = started
	} else {
		record.LastError = failure.Error()
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return h.write(h.backupRecordPath(name), append(data, '\n'), 0600)
}

func backupDue(a config.App, record backupRecord, now time.Time) bool {
	interval := a.Backup.Interval()
	return interval > 0 && (record.LastSuccess.IsZero() || !now.Before(record.LastSuccess.Add(interval-backupSlack)))
}

func (h Host) dueBackups(c config.Config, now time.Time) ([]config.App, error) {
	var due []config.App
	for _, a := range c.Apps {
		record, err := h.loadBackupRecord(a.Name)
		if err != nil {
			return nil, err
		}
		if backupDue(a, record, now) {
			due = append(due, a)
		}
	}
	return due, nil
}

// BackupStatus reports each app's schedule, retention and last outcome.
func (h Host) BackupStatus() error {
	c, err := config.Load(h.Manager.ConfigPath)
	if err != nil {
		return err
	}
	if len(c.Apps) == 0 {
		return fmt.Errorf("no registered apps")
	}
	scheduled := false
	now := time.Now()
	stamp := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	status := table.Table{
		Title:   "Backups · times in " + now.Local().Format("MST"),
		Headers: []string{"APP", "SCHEDULE", "KEEP", "LAST BACKUP", "NEXT", "RESULT"},
	}
	var failures []string
	for _, a := range c.Apps {
		record, err := h.loadBackupRecord(a.Name)
		if err != nil {
			return err
		}
		schedule := table.Styled("manual", table.Muted)
		if a.Backup.Schedule != "" {
			scheduled = true
			schedule = table.Text(map[string]string{"daily": "daily", "weekly": "weekly"}[a.Backup.Schedule])
			if schedule.Text == "" {
				schedule.Text = "every " + a.Backup.Schedule
			}
		}
		keep := table.Styled("all", table.Muted)
		if a.Backup.Keep > 0 {
			keep = table.Text("newest " + strconv.Itoa(a.Backup.Keep))
		}
		last := table.Styled("never", table.Muted)
		if !record.LastSuccess.IsZero() {
			last = table.Text(stamp(record.LastSuccess))
		} else if a.Backup.Schedule != "" {
			last.Tone = table.Warn
		}
		next := table.Styled("—", table.Muted)
		if a.Backup.Schedule != "" {
			if backupDue(a, record, now) {
				next = table.Styled("due now", table.Warn)
			} else {
				// The timer fires on local hours, which may be offset from UTC.
				ready := record.LastSuccess.Add(a.Backup.Interval() - backupSlack).Local()
				at := time.Date(ready.Year(), ready.Month(), ready.Day(), ready.Hour(), 0, 0, 0, time.Local)
				if at.Before(ready) {
					at = at.Add(time.Hour)
				}
				next = table.Text(stamp(at))
			}
		}
		result := table.Styled("—", table.Muted)
		if record.LastError != "" {
			result = table.Styled("failed", table.Bad)
			failures = append(failures, fmt.Sprintf("%s: last attempt failed at %s: %s", a.Name, stamp(record.LastAttempt), record.LastError))
		} else if !record.LastSuccess.IsZero() {
			result = table.Styled("ok", table.Good)
		}
		status.Rows = append(status.Rows, []table.Cell{table.Styled(a.Name, table.Strong), schedule, keep, last, next, result})
	}
	if err := h.writeTable(status); err != nil {
		return err
	}
	for _, failure := range failures {
		h.say("%s", failure)
	}
	if scheduled && !h.DryRun {
		out, err := h.probe(Command{Name: "systemctl", Args: []string{"is-active", "abr-backup.timer"}, Private: true})
		if err != nil || strings.TrimSpace(string(out)) != "active" {
			h.say("abr-backup.timer is not active; run abr setup to install it")
		}
	}
	return nil
}

// ensureBackupTimer installs the hourly timer that runs due app backups with
// this executable and these paths. It never replaces unmanaged units.
func (h Host) ensureBackupTimer() error {
	if h.DryRun {
		h.say("Would install and enable the hourly abr-backup.timer for scheduled app backups")
		return nil
	}
	binary, err := os.Executable()
	if err == nil {
		binary, err = filepath.EvalSymlinks(binary)
	}
	if err != nil {
		return err
	}
	data := map[string]string{"Binary": binary}
	for name, value := range map[string]string{"Config": h.Manager.ConfigPath, "StateDir": h.Manager.StateDir, "TemplatesDir": h.TemplatesDir, "AppsDir": h.AppsDir} {
		if data[name], err = filepath.Abs(value); err != nil {
			return err
		}
	}
	changed := false
	for _, unit := range []struct{ source, target string }{{"backup.service.tmpl", "/etc/systemd/system/abr-backup.service"}, {"backup.timer.tmpl", "/etc/systemd/system/abr-backup.timer"}} {
		t, err := template.New(unit.source).Option("missingkey=error").Funcs(template.FuncMap{
			"unit": func(s string) (string, error) {
				if strings.ContainsAny(s, "\x00\r\n") {
					return "", fmt.Errorf("unsupported characters in systemd path")
				}
				return strconv.Quote(strings.ReplaceAll(s, "%", "%%")), nil
			},
		}).ParseFiles(filepath.Join(h.TemplatesDir, unit.source))
		if err != nil {
			return err
		}
		var rendered bytes.Buffer
		if err := t.Execute(&rendered, data); err != nil {
			return err
		}
		if !managed(rendered.Bytes()) {
			return fmt.Errorf("%s must start with the abr managed marker", unit.source)
		}
		old, err := h.read(unit.target)
		if err == nil && !managed(old) {
			return fmt.Errorf("refusing to replace unmanaged unit %s", unit.target)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if bytes.Equal(old, rendered.Bytes()) {
			continue
		}
		if err := h.write(unit.target, rendered.Bytes(), 0644); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if err := h.command("systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	return h.command("systemctl", "enable", "--now", "abr-backup.timer")
}
