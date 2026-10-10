package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"abr/internal/backup"
	"abr/internal/config"
	"abr/internal/manager"
	"abr/internal/ports"
)

type BackupOptions struct {
	All       bool
	OutputDir string
	Remote    string
	RemoteDir string
	SSHPort   int
	KeyFile   string
	Password  string
	Transfer  bool
}

func (o BackupOptions) Validate(names []string) error {
	if o.All == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
	}
	if o.OutputDir != "" && (!filepath.IsAbs(o.OutputDir) || strings.ContainsAny(o.OutputDir, "\x00\r\n")) {
		return fmt.Errorf("use an absolute --output-dir")
	}
	if (o.Remote == "") != (o.RemoteDir == "") {
		return fmt.Errorf("--remote and --remote-dir must be used together")
	}
	if o.SSHPort < 0 || o.SSHPort > 65535 {
		return fmt.Errorf("invalid backup SSH port")
	}
	if o.Remote != "" {
		if !regexp.MustCompile(`^[a-z_][a-z0-9_-]*@[a-zA-Z0-9][a-zA-Z0-9.:-]*$`).MatchString(o.Remote) {
			return fmt.Errorf("use --remote USER@HOST (SSH key authentication)")
		}
		if !path.IsAbs(o.RemoteDir) || path.Clean(o.RemoteDir) != o.RemoteDir || o.RemoteDir == "/" || strings.ContainsAny(o.RemoteDir, "\x00\r\n") {
			return fmt.Errorf("use a clean absolute --remote-dir outside /")
		}
	} else if o.SSHPort != 0 {
		return fmt.Errorf("--ssh-port requires --remote")
	}
	return nil
}

// BackupApps completes capture, transfer and local cleanup before the next app.
// App and shared services remain running throughout capture and transfer.
func (h Host) BackupApps(names []string, o BackupOptions) error {
	if err := o.Validate(names); err != nil {
		return err
	}
	return h.locked(func() error {
		var err error
		o, err = h.backupOptions(o)
		if err != nil {
			return err
		}
		if o.Transfer && o.Remote == "" {
			return fmt.Errorf("configure a backup destination in the menu or with abr backup configure before transferring")
		}
		capture := func(c config.Config, r ports.Registry) error {
			if err := manager.CheckReservations(c, r); err != nil {
				return err
			}
			selected, err := selectBackupApps(c, names, o.All)
			if err != nil {
				return err
			}
			if h.DryRun {
				for _, a := range selected {
					h.say("Would back up %s online: managed SQL, .env, full Laravel storage/app and saved source and recovery settings into %s; services remain running", a.Name, o.OutputDir)
					if o.Remote != "" {
						h.say("Would transfer %s to %s:%s, verify SHA256 and remote publication, then remove only its local archive before the next app", a.Name, o.Remote, o.RemoteDir)
					}
				}
				h.say("App backups exclude shared Redis persistence and Caddy certificate storage; no files changed")
				return nil
			}
			if err := h.prepareAppBackupDirectory(c, o.OutputDir); err != nil {
				return err
			}
			// One small shared snapshot gives all archives identical recovery settings.
			shared, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".app-backup-shared-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(shared)
			settings, err := h.backupSettings()
			if err != nil {
				return err
			}
			for _, name := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json", "mysql-admin.json"} {
				if err := copyBackupFile(h.path(filepath.Join(h.Manager.StateDir, name)), filepath.Join(shared, "state", name), name != "git/id_ed25519" && name != "git/known_hosts"); err != nil {
					return err
				}
			}
			if err := h.copyBackupTemplates(filepath.Join(shared, "templates")); err != nil {
				return err
			}
			for name, source := range sharedBackupFiles() {
				if err := copyBackupFile(h.path(source), filepath.Join(shared, name), true); err != nil {
					return err
				}
			}
			stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
			for _, a := range selected {
				output := filepath.Join(o.OutputDir, a.Name+"-"+stamp+".tar.gz")
				if err := h.captureAppBackup(a, c, r, settings, shared, output); err != nil {
					return fmt.Errorf("backup %s: %w", a.Name, err)
				}
				if o.Remote == "" {
					h.say("App backup complete: %s (0600; contains secrets)", output)
					continue
				}
				if err := h.transferAppBackup(output, o); err != nil {
					return fmt.Errorf("transfer %s failed; local backup retained at %s: %w", a.Name, output, err)
				}
				if err := os.Remove(output); err != nil {
					return fmt.Errorf("remote backup verified; local cleanup failed at %s: %w", output, err)
				}
				h.say("Backed up %s to %s:%s; verified SHA256; local archive removed", a.Name, o.Remote, path.Join(o.RemoteDir, filepath.Base(output)))
			}
			return nil
		}
		if h.DryRun {
			c, err := config.Load(h.Manager.ConfigPath)
			if err != nil {
				return err
			}
			r, err := ports.Load(h.Manager.RegistryPath())
			if err != nil {
				return err
			}
			return capture(c, r)
		}
		return h.Manager.WithSnapshot(capture)
	})
}

func selectBackupApps(c config.Config, names []string, all bool) ([]config.App, error) {
	if all {
		if len(c.Apps) == 0 {
			return nil, fmt.Errorf("no registered apps to back up")
		}
		return c.Apps, nil
	}
	var selected []config.App
	seen := map[string]bool{}
	for _, name := range names {
		index := slices.IndexFunc(c.Apps, func(a config.App) bool { return a.Name == name })
		if index < 0 || seen[name] {
			return nil, fmt.Errorf("unknown or duplicate backup app %q", name)
		}
		seen[name] = true
		selected = append(selected, c.Apps[index])
	}
	return selected, nil
}

func (h Host) prepareAppBackupDirectory(c config.Config, directory string) error {
	for _, source := range append([]string{h.Manager.StateDir, h.TemplatesDir, h.AppsDir, h.path("/var/lib/redis"), h.path("/var/lib/caddy/.local/share/caddy")}, appDirectories(c)...) {
		if within(source, directory) {
			return fmt.Errorf("backup output must be outside managed data directories")
		}
	}
	if err := h.trustedAncestor(directory); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := h.trustedDirectory(directory); err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("backup output directory must have mode 0700")
	}
	return nil
}

func (h Host) captureAppBackup(a config.App, c config.Config, r ports.Registry, settings backup.Settings, shared, output string) error {
	c.Apps = []config.App{a}
	appPorts := ports.Empty()
	for _, assignment := range r.Assignments {
		if assignment.App == a.Name {
			appPorts.Assignments = append(appPorts.Assignments, assignment)
		}
	}
	records, err := h.backupRepositories(c)
	if err != nil {
		return err
	}
	var creds *credentials
	_, err = os.Lstat(h.path(h.credentialsPath(a.Name)))
	if os.IsNotExist(err) && a.Type == "laravel" {
		return fmt.Errorf("full app backup requires recorded managed MySQL credentials")
	}
	if !os.IsNotExist(err) || a.Database.Enabled {
		dbApp := a
		dbApp.Database.Enabled = true
		record, err := h.transferCredentials(dbApp)
		if err != nil {
			return err
		}
		creds = &record
	}
	if a.Type == "laravel" {
		if _, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env"))); err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".app-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := backup.CopyTree(shared, stage); err != nil {
		return err
	}
	if err := h.captureBackupCode(a, records[0], stage); err != nil {
		return err
	}
	if creds != nil {
		if err := os.MkdirAll(filepath.Join(stage, "sql"), 0700); err != nil {
			return err
		}
		if err := h.dumpDatabase(*creds, filepath.Join(stage, "sql", a.Name+".sql")); err != nil {
			return err
		}
		data, err := json.Marshal(creds)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(stage, "state/databases"), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, "state/databases", a.Name+".json"), data, 0600); err != nil {
			return err
		}
	}
	if err := copyBackupFile(h.path(filepath.Join(a.Directory, ".env")), filepath.Join(stage, "apps", a.Name, ".env"), a.Type == "nuxt"); err != nil {
		return err
	}
	if a.Type == "laravel" {
		if err := copyBackupTree(h.path(filepath.Join(a.Directory, "storage/app")), filepath.Join(stage, "apps", a.Name, "storage/app"), true); err != nil {
			return err
		}
	}
	m := backup.Manifest{Version: 1, Scope: "app", CreatedAt: time.Now().UTC(), Config: c, Ports: appPorts, Apps: records, Settings: settings}
	f, err := os.CreateTemp(filepath.Dir(output), ".abr-app-backup-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := backup.Write(f, stage, &m); err != nil {
		return err
	}
	if err := validateFullBackup(m, stage); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), output); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(output))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// The receiver publishes by hard link only after hashing and syncing the upload.
// SSH's exit status alone is insufficient: an exact receipt is required too.
const backupReceiver = `set -eu
umask 077
directory=$1
name=$2
expected=$3
test -d "$directory"
test "$(realpath -e -- "$directory")" = "$directory"
test "$(stat -c '%u:%a' -- "$directory")" = "$(id -u):700"
cd -- "$directory"
temporary=$(mktemp .abr-upload-XXXXXXXXXX)
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
actual=$(sha256sum -- "$temporary")
actual=${actual%% *}
test "$actual" = "$expected"
sync -f "$temporary"
ln -- "$temporary" "$name"
rm -f -- "$temporary"
sync -f .
printf 'ABR_BACKUP_OK %s %s\n' "$actual" "$name"
`

func (h Host) transferAppBackup(file string, o BackupOptions) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	name := filepath.Base(file)
	command := "sh -c " + shellQuote(backupReceiver) + " abr-backup " + shellQuote(o.RemoteDir) + " " + shellQuote(name) + " " + shellQuote(checksum)
	out, err := h.backupSSH(o, command, f)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "ABR_BACKUP_OK "+checksum+" "+name {
		return fmt.Errorf("remote verification receipt missing or invalid")
	}
	return nil
}

func (h Host) backupSSH(o BackupOptions, command string, input io.Reader) ([]byte, error) {
	port := o.SSHPort
	if port == 0 {
		port = 22
	}
	batch := "yes"
	if o.Password != "" {
		batch = "no"
	}
	args := []string{"-F", "/dev/null", "-T", "-o", "BatchMode=" + batch, "-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-p", strconv.Itoa(port)}
	if o.KeyFile != "" {
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", o.KeyFile)
	}
	if o.Password != "" {
		args = append(args, "-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no", "-o", "NumberOfPasswordPrompts=1")
	}
	c := Command{Name: "ssh", Args: append(args, "--", o.Remote, command), Stdin: input, Private: true}
	if o.Password != "" {
		secret, err := os.CreateTemp(h.path(h.Manager.StateDir), ".ssh-password-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(secret.Name())
		defer secret.Close()
		if _, err := io.WriteString(secret, o.Password+"\n"); err != nil {
			return nil, err
		}
		if _, err := secret.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		c.Name, c.Args, c.ExtraFiles = "sshpass", append([]string{"-d", "3", "/usr/bin/ssh"}, c.Args...), []*os.File{secret}
	}
	return h.run("Transfer or verify private backup destination", c)
}
