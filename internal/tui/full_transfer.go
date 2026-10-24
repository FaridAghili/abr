package tui

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"abr/internal/backup"

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
		case "status":
			return m.start(action{title: "Backup status", args: []string{"backup", "status"}})
		}
		return m.serverMenu()
	}, huh.NewGroup(huh.NewSelect[string]().Title("Backup actions").Description("Preserve source, data and settings; restore apps here, or a whole server onto a fresh one.").Options(
		huh.NewOption("Back up apps and transfer", "backup"),
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

func (m *model) appBackupForm() tea.Cmd {
	var names string
	return m.setForm("form", "Back up apps", func() tea.Cmd {
		args := []string{"backup", "--transfer"}
		if strings.TrimSpace(names) == "" {
			args = append(args, "--all")
		} else {
			args = append(args, strings.Fields(names)...)
		}
		return m.review(action{title: "Back up apps", args: args, note: "Keep all apps running. Capture one app's source and lockfiles, SQL, .env, full Laravel storage/app and recovery settings, compress, transfer to the configured backup server, verify SHA256, then remove its local archive before the next app. Failed transfers retain their archive and stop the run; the next successful backup of that app uploads and removes it, leaving no archive on this server. Redis data is excluded. HTTPS is recreated from saved domains/settings on restore. Avoid schema changes; SQL and files may differ in time."})
	}, huh.NewGroup(textInput("Apps (optional)", "Space-separated registered app names. Blank backs up every app, one at a time.", "api portal", &names).Validate(func(s string) error {
		for _, name := range strings.Fields(s) {
			if err := applicationName(name); err != nil {
				return err
			}
		}
		return nil
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
