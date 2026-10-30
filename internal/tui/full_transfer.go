package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"abr/internal/backup"
	"abr/internal/config"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func (m *model) fullTransferMenu() tea.Cmd {
	m.menu, m.back = m.fullTransferMenu, m.serverMenu
	var selected string
	return m.setForm("menu", "Backup & restore", func() tea.Cmd {
		switch selected {
		case "backup":
			return m.appBackupForm()
		case "configure":
			return m.backupDestinationForm()
		case "test":
			return m.backupTestReview()
		case "full":
			return m.fullBackupForm()
		case "restore":
			return m.fullRestoreForm()
		case "schedules":
			return m.backupScheduleForm()
		case "status":
			return m.start(action{title: "Backup status", args: []string{"backup", "status"}})
		}
		return m.serverMenu()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Backup actions").Description("Preserve source, data and settings; restore apps here, or a whole server onto a fresh one.").Options(
		huh.NewOption("Back up apps now", "backup"),
		huh.NewOption("Backup schedules · many apps at once", "schedules"),
		huh.NewOption("Backup status", "status"),
		huh.NewOption("Configure backup server", "configure"),
		huh.NewOption("Test backup destination", "test"),
		huh.NewOption("Full server archive (local)", "full"),
		huh.NewOption("Restore apps or a server", "restore"),
		huh.NewOption("Back", "back")).Value(&selected)))
}

func (m *model) backupTestReview() tea.Cmd {
	if m.options.BackupHostKey == nil || m.options.BackupTrust == nil {
		return m.workflowError("Test backup destination", errors.New("Backup server identity verification is unavailable"))
	}
	var key backup.HostKey
	inspect, trust, run := m.options.BackupHostKey, m.options.BackupTrust, m.options.RunCommand
	return m.review(action{title: "Test backup destination", args: []string{"backup", "host-key"}, note: "Check the backup server identity, then verify SSH access and the private backup directory. A new host key requires your fingerprint confirmation. No app services are stopped.", run: func(output io.Writer) error {
		var err error
		key, err = inspect(output)
		return err
	}, after: func() tea.Cmd {
		if key.Known {
			return m.start(action{title: "Test backup destination", args: []string{"backup", "test"}})
		}
		return m.review(action{title: "Trust backup server and test", args: []string{"backup", "trust", "--fingerprint", key.Fingerprint}, note: fmt.Sprintf("Backup server: %s\nKey type: ED25519\nFingerprint: %s\n\nCompare this fingerprint with the output on your backup server:\nssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\n\nConfirm only if they match. Save this host key in root's known_hosts, then test SSH access and the private directory. No manual SSH login is needed.", key.Address, key.Fingerprint), run: func(output io.Writer) error {
			if err := trust(key.Fingerprint, output); err != nil {
				return err
			}
			return run([]string{"backup", "test"}, output)
		}})
	}})
}

func archivePath(s string) error {
	if !filepath.IsAbs(s) || !strings.HasSuffix(s, ".tar.gz") || strings.ContainsAny(s, "\x00\r\n") {
		return errors.New("Use an absolute .tar.gz file path")
	}
	return nil
}

func (m *model) fullBackupForm() tea.Cmd {
	output := "/var/backups/abr/full-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	return m.setForm("form", "Full backup", func() tea.Cmd {
		return m.review(action{title: "Full backup", args: []string{"backup", "--output", output}, note: fmt.Sprintf("Archive: %s\n\nCapture all managed MySQL databases, an online Redis snapshot, saved source and lockfiles, .env files, full Laravel storage/app, Caddy storage, settings, templates and credentials. All services keep running. SQL and files may differ in time; avoid schema changes during capture. The private archive contains secrets and stays local.", output)})
	}, huh.NewGroup(textInput("Backup archive", "Absolute output path outside app/state directories. Existing files are preserved.", "/var/backups/abr/full.tar.gz", &output).Validate(archivePath)))
}

func (m *model) fullRestoreForm() tea.Cmd {
	var input, name string
	var rename bool
	return m.setForm("form", "Restore", func() tea.Cmd {
		files, err := restorePaths(input)
		if err != nil {
			return m.workflowError("Restore", err)
		}
		args := append([]string{"restore"}, files...)
		args = append(args, "--yes")
		note := fmt.Sprintf("Backup: %s\n\n", input)
		if rename {
			if len(files) != 1 {
				return m.workflowError("Restore", fmt.Errorf("A new name restores one app backup; %s holds backups of %d apps. Enter that app's .tar.gz file instead.", input, len(files)))
			}
			args = append(args, "--name", name)
			note = fmt.Sprintf("Backup: %s\nRestore as: %s\nProject: %s\n\nThe backup's app is renamed to %s. Its Linux user, MySQL database and account, services, ports and records use the new name, and DB_DATABASE and DB_USERNAME in its .env are updated. Domains, secrets, uploads and data are kept as saved. An app named %s on this server is refused, so other apps stay unchanged.\n\n", input, name, filepath.Join(m.options.AppsDir, name), name, name)
		}
		return m.review(action{title: "Restore", args: args, note: note + "Confirm SQL and Redis imports when included. On a server that is already set up, app backups are added as new apps; its settings, other apps and Redis data stay unchanged, and an app or database that already exists is refused. A fresh Ubuntu server with working SSH key access is provisioned from the backup's settings first; a full server archive restores only there. Recreate users/accounts, restore the bundled source and lockfiles, restore .env/uploads/data, build, migrate and optimize. App backups exclude shared Redis data and Caddy certificates. Previously enabled apps start only after all builds succeed. Keep the old server’s workers stopped during cutover."})
	}, huh.NewGroup(textInput("Backup file or directory", "Absolute archive path, or a directory of app backups; the newest backup of each app is used. Copy the backups here first.", "/var/backups/abr/restore", &input).Validate(func(s string) error {
		if !filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n") {
			return errors.New("Use an absolute backup file or directory")
		}
		return nil
	})),
		huh.NewGroup(huh.NewSelect[bool]().Title("App name").Description("Keep the saved name, or restore one app under a new name when this server already has an app with that name.\nExample: restore api as api-old beside the existing api.").Options(huh.NewOption("Keep the backup's name (default)", false), huh.NewOption("Restore under a new name", true)).Value(&rename)),
		huh.NewGroup(textInput("New app name", "Name for the restored app; its project, user, database and services follow it. Domains are kept.", "api-old", &name).Validate(applicationName)).WithHideFunc(func() bool { return !rename }))
}

func restorePaths(input string) ([]string, error) {
	if strings.HasSuffix(input, ".tar.gz") {
		return []string{input}, archivePath(input)
	}
	return backup.LatestArchives(input)
}

// backupServer reports whether a backup server is saved; it only checks that
// the private settings file exists, which the backup command then validates.
func (m *model) backupServer() bool {
	_, err := os.Stat(filepath.Join(m.options.StateDir, "backup-destination.json"))
	return err == nil
}

// appChoices lists apps by name with their current backup plan.
func (m *model) appChoices() ([]huh.Option[string], error) {
	c, err := config.Load(m.options.ConfigPath)
	if err != nil {
		return nil, err
	}
	if len(c.Apps) == 0 {
		return nil, errors.New("No apps registered. Choose Add application from the main menu.")
	}
	slices.SortFunc(c.Apps, func(a, b config.App) int { return strings.Compare(a.Name, b.Name) })
	choices := make([]huh.Option[string], 0, len(c.Apps))
	for _, app := range c.Apps {
		choices = append(choices, huh.NewOption(clean(app.Name+" · "+backupPlan(app.Backup)), app.Name))
	}
	return choices, nil
}

func scheduleLabel(schedule string) string {
	switch schedule {
	case "", "off":
		return "manual"
	case "daily", "weekly":
		return schedule
	}
	return "every " + schedule
}

func backupPlan(b config.Backup) string {
	plan := scheduleLabel(b.Schedule)
	if b.Keep > 0 {
		return plan + ", keep " + strconv.Itoa(b.Keep)
	}
	return plan + ", keep all"
}

func selectApps(title, description string, choices []huh.Option[string], selected *[]string) *huh.MultiSelect[string] {
	return huh.NewMultiSelect[string]().Height(min(len(choices)+2, 12)).Title(title).Description(description).Options(choices...).Value(selected).Validate(func(v []string) error {
		if len(v) == 0 {
			return errors.New("Select at least one app with Space")
		}
		return nil
	})
}

func (m *model) appBackupForm() tea.Cmd {
	choices, err := m.appChoices()
	if err != nil {
		return m.workflowError("Back up apps", err)
	}
	var names []string
	server := m.backupServer()
	local := !server
	serverLabel := "Backup server"
	if !server {
		serverLabel += " (not configured yet)"
	}
	directory := "/var/backups/abr/apps"
	return m.setForm("form", "Back up apps", func() tea.Cmd {
		args := []string{"backup", "--transfer"}
		note := "Keep all apps running. Capture one app's source and lockfiles, SQL, .env, full Laravel storage/app and recovery settings, compress, transfer to the configured backup server, verify SHA256, then remove its local archive before the next app. Failed transfers retain their archive and stop the run; the next successful backup of that app uploads and removes it, leaving no archive on this server."
		if local {
			args = []string{"backup", "--local", "--output-dir", directory}
			note = fmt.Sprintf("Directory: %s\n\nKeep all apps running. Capture one app's source and lockfiles, SQL, .env, full Laravel storage/app and recovery settings into a private archive (0600) in this directory, one app at a time. Older archives beyond each app's retention are deleted. Archives contain secrets and are not encrypted; copy them off this server, since a lost server loses them too.", directory)
		}
		args = append(args, names...)
		return m.review(action{title: "Back up apps", args: args, note: fmt.Sprintf("Apps: %s\n", strings.Join(names, ", ")) + note + " Redis data is excluded. HTTPS is recreated from saved domains/settings on restore. Avoid schema changes; SQL and files may differ in time."})
	}, huh.NewGroup(selectApps("Apps", "Space selects an app, Ctrl+A selects all; type to filter.\nExample: select api and portal to back up both.", choices, &names)),
		huh.NewGroup(huh.NewSelect[bool]().Title("Where to keep the backups?").Description("Send them to your backup server, or keep them on this server.\nExample: this server for a quick copy before a risky change.").Options(huh.NewOption(serverLabel, false), huh.NewOption("This server · private directory", true)).Value(&local)),
		huh.NewGroup(textInput("Backup directory", "Absolute private directory on this server, outside app and Abr directories.", "/var/backups/abr/apps", &directory).Validate(func(s string) error {
			if !filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n") {
				return errors.New("Use an absolute directory")
			}
			return nil
		})).WithHideFunc(func() bool { return !local }))
}

// backupScheduleForm sets one schedule for a group of apps, then offers the
// next group, so different plans take a few steps each instead of per-app menus.
func (m *model) backupScheduleForm() tea.Cmd {
	m.menu, m.back = m.fullTransferMenu, m.fullTransferMenu
	choices, err := m.appChoices()
	if err != nil {
		return m.workflowError("Backup schedules", err)
	}
	var names []string
	schedule, hours, keep := "daily", "6", ""
	where := "the backup server"
	if !m.backupServer() {
		where = "this server (/var/backups/abr/apps) until a backup server is configured"
	}
	return m.setForm("form", "Backup schedules", func() tea.Cmd {
		value := schedule
		if schedule == "hours" {
			value = hours + "h"
		}
		args := append(append([]string{"backup", "schedule"}, names...), "--schedule", value)
		retention := "Each app keeps its current retention."
		if strings.TrimSpace(keep) != "" {
			args = append(args, "--keep", strings.TrimSpace(keep))
			retention = "Keep the newest " + strings.TrimSpace(keep) + " backups of each app here and on the backup server; older ones are deleted (0 keeps all)."
		}
		note := fmt.Sprintf("Apps: %s\nSchedule: %s\n\n%s Scheduled backups go to %s. An hourly timer backs each app up when due; services keep running and no deploy is needed.\n\nAfter saving, press Enter to set the next group of apps, or Esc to finish.", strings.Join(names, ", "), scheduleLabel(value), retention, where)
		return m.review(action{title: "Save backup schedules", args: args, note: note, continueWith: m.backupScheduleForm})
	}, huh.NewGroup(selectApps("Apps with the same plan", "Choose a group sharing one schedule; set the next group after saving. Space selects, Ctrl+A all.\nExample: all small sites now, then busy shops every few hours.", choices, &names)),
		huh.NewGroup(huh.NewSelect[string]().Title("How often?").Description("Automatic backups for the selected apps. Services keep running.\nExample: daily for a small site, every few hours for busy data.").Options(huh.NewOption("Daily", "daily"), huh.NewOption("Every few hours", "hours"), huh.NewOption("Weekly", "weekly"), huh.NewOption("Off (manual only)", "off")).Value(&schedule)),
		huh.NewGroup(textInput("Hours between backups", "Back up every this many hours, from 1 to 23.", "6", &hours).Validate(func(s string) error {
			return (config.Backup{Schedule: s + "h"}).Validate()
		})).WithHideFunc(func() bool { return schedule != "hours" }),
		huh.NewGroup(textInput("Backups to keep (optional)", "Newest backups kept per app; older ones are deleted. 0 keeps all; blank keeps each app's current number.", "7", &keep).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return errors.New("Enter a number from 0 to 1000, or leave blank")
			}
			return (config.Backup{Keep: n}).Validate()
		})))
}

func (m *model) backupDestinationForm() tea.Cmd {
	d := backup.Destination{Port: 22, Auth: "key", Directory: "/srv/backups/abr"}
	var key string
	port := "22"
	var advanced bool
	return m.setForm("form", "Configure backup server", func() tea.Cmd {
		if d.Auth == "key" {
			d.Password = ""
		} else {
			key = ""
		}
		parsed, err := strconv.Atoi(port)
		if err != nil {
			return m.workflowError("Backup configuration", err)
		}
		d.Port = parsed
		if err := d.Validate(); err != nil {
			return m.workflowError("Backup configuration", err)
		}
		if m.options.BackupConfigure == nil {
			return m.workflowError("Backup configuration", errors.New("Backup destination saving is unavailable"))
		}
		save := m.options.BackupConfigure
		return m.review(action{title: "Save backup server", args: []string{"backup", "configure", "--host", d.Host, "--user", d.User, "--path", d.Directory}, note: fmt.Sprintf("Server: %s@%s\nDirectory: %s\nSSH port: %d\nAuthentication: %s\n\nSave private connection settings. Key mode generates a dedicated key when no private key is supplied, then displays its public key for installation on the backup server. Password mode installs sshpass and stores the password in root-only state. Create the destination with mode 0700, then choose Test backup destination to confirm its host fingerprint and check access.", d.User, d.Host, d.Directory, d.Port, d.Auth), run: func(output io.Writer) error {
			defer func() { d.Password = "" }()
			return save(d, key, output)
		}})
	}, huh.NewGroup(textInput("Server IP / hostname", "Address of your Ubuntu backup server, without ssh://.", "192.0.2.10", &d.Host).Validate(required)),
		huh.NewGroup(textInput("SSH user", "Existing account that owns the remote backup directory.", "backup", &d.User).Validate(required)),
		huh.NewGroup(textInput("Backup directory", "Absolute directory on the backup server, owned by the SSH user with mode 0700.", "/srv/backups/abr", &d.Directory).Validate(required)),
		huh.NewGroup(huh.NewSelect[string]().Title("Authentication").Description("Use SSH keys for unattended backups, or a saved password.\nExample: SSH key displays a public key to install on the backup server.").Options(huh.NewOption("SSH key (recommended)", "key"), huh.NewOption("Password", "password")).Value(&d.Auth)),
		huh.NewGroup(textInput("SSH password", "Password for the backup server account; stored privately and hidden here.", "your SSH password", &d.Password).EchoMode(huh.EchoModePassword).Validate(required)).WithHideFunc(func() bool { return d.Auth != "password" }),
		huh.NewGroup(huh.NewSelect[bool]().Title("Advanced settings?").Description("Default port is 22 and key mode generates a dedicated identity.\nExample: import a private key or use SSH port 2222.").Options(huh.NewOption("Skip (default)", false), huh.NewOption("Configure extras", true)).Value(&advanced)),
		huh.NewGroup(textInput("SSH port", "Port used to connect to the backup server.", "22", &port).Validate(func(s string) error {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("Use port 1–65535")
			}
			return nil
		})).WithHideFunc(func() bool { return !advanced }),
		huh.NewGroup(textInput("Private key file (optional)", "Import an unencrypted private key. Blank creates or reuses Abr's backup key; its public key is displayed.", "/root/.ssh/backup_ed25519", &key)).WithHideFunc(func() bool { return !advanced || d.Auth != "key" }))
}
