package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Due       bool
	OutputDir string
	Remote    string
	RemoteDir string
	SSHPort   int
	KeyFile   string
	Password  string
	Transfer  bool
	Local     bool // Keep archives in OutputDir even when a backup server is saved.
}

func (o BackupOptions) Validate(names []string) error {
	if o.Due {
		if o.All || len(names) > 0 {
			return fmt.Errorf("--due selects apps by their backup schedule; do not combine it with APP... or --all")
		}
	} else if o.All == (len(names) > 0) {
		return fmt.Errorf("select APP... or --all")
	}
	if o.OutputDir != "" && (!filepath.IsAbs(o.OutputDir) || strings.ContainsAny(o.OutputDir, "\x00\r\n")) {
		return fmt.Errorf("use an absolute --output-dir")
	}
	if o.Local && (o.Transfer || o.Remote != "") {
		return fmt.Errorf("--local keeps backups on this server; do not combine it with --transfer or --remote")
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

// BackupApps completes capture, transfer and cleanup before the next app. Only
// each app's capture holds the host lock, so other abr commands wait at most
// for one capture, never for transfers. App and shared services keep running.
// Due runs continue after a failed app and report every failure at the end.
func (h Host) BackupApps(names []string, o BackupOptions) error {
	if err := o.Validate(names); err != nil {
		return err
	}
	var selected []config.App
	err := h.locked(func() error {
		var err error
		if o, err = h.backupOptions(o); err != nil {
			return err
		}
		if o.Transfer && o.Remote == "" {
			return fmt.Errorf("no backup server is configured; configure one in the menu or with abr backup configure, or back up locally with --local")
		}
		c, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		if o.Due {
			selected, err = h.dueBackups(c, time.Now())
		} else {
			selected, err = selectBackupApps(c, names, o.All)
		}
		return err
	})
	if err != nil {
		return err
	}
	if o.Due && len(selected) == 0 {
		h.say("No scheduled app backups are due")
		return nil
	}
	if h.DryRun {
		for _, a := range selected {
			h.say("Would back up %s online: managed SQL, .env, full Laravel storage/app and saved source and recovery settings into %s; services remain running", a.Name, o.OutputDir)
			if o.Remote != "" {
				h.say("Would transfer %s to %s:%s, verify SHA256 and remote publication, then remove its local archive and any earlier ones left by failed transfers before the next app", a.Name, o.Remote, o.RemoteDir)
			}
			if a.Backup.Keep > 0 {
				h.say("Would keep the newest %d backups of %s and delete older ones", a.Backup.Keep, a.Name)
			}
		}
		h.say("App backups exclude shared Redis persistence and Caddy certificate storage; no files changed")
		return nil
	}
	var failures []error
	for _, a := range selected {
		if err := h.backupApp(a.Name, o); err != nil {
			if !o.Due {
				return err
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
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

// backupApp captures one app under the host lock, then transfers, prunes and
// records the result without holding it.
func (h Host) backupApp(name string, o BackupOptions) (result error) {
	started := time.Now().UTC()
	defer func() { result = errors.Join(result, h.recordBackup(name, started, result)) }()
	var output, checksum string
	keep := 0
	err := h.locked(func() error {
		// Archive names have one-second resolution. Captures are serialized by
		// the host lock, so holding it into the next second keeps names unique.
		started = time.Now().UTC()
		defer func() {
			if wait := time.Until(started.Truncate(time.Second).Add(time.Second)); wait > 0 {
				time.Sleep(wait)
			}
		}()
		return h.Manager.WithSnapshot(func(c config.Config, r ports.Registry) error {
			if err := manager.CheckReservations(c, r); err != nil {
				return err
			}
			index := slices.IndexFunc(c.Apps, func(a config.App) bool { return a.Name == name })
			if index < 0 {
				return fmt.Errorf("unknown application %q", name)
			}
			a := c.Apps[index]
			keep = a.Backup.Keep
			if err := h.prepareAppBackupDirectory(c, o.OutputDir); err != nil {
				return err
			}
			shared, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".app-backup-shared-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(shared)
			settings, err := h.stageSharedBackup(shared)
			if err != nil {
				return err
			}
			output = filepath.Join(o.OutputDir, a.Name+"-"+started.Format("20060102150405")+".tar.gz")
			checksum, err = h.captureAppBackup(a, c, r, settings, shared, output)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("backup %s: %w", name, err)
	}
	// Bound local archives too, including ones retained after failed transfers.
	if err := h.pruneLocalBackups(o.OutputDir, name, keep, filepath.Base(output)); err != nil {
		return fmt.Errorf("backup %s saved at %s; local cleanup failed: %w", name, output, err)
	}
	if o.Remote == "" {
		h.say("App backup complete: %s (0600; contains secrets)", output)
		return nil
	}
	// Archives kept by earlier failed transfers go first, oldest first, so each
	// upload is the newest on the backup server and its retention stays exact.
	// A verified transfer leaves no local archive of this app on the server.
	pending, err := h.pendingBackups(o.OutputDir, name, filepath.Base(output))
	if err != nil {
		return fmt.Errorf("backup %s saved at %s; listing earlier local backups failed: %w", name, output, err)
	}
	removed := 0
	for _, file := range append(pending, output) {
		sum := checksum
		if file != output {
			if sum, err = fileChecksum(file); err != nil {
				return fmt.Errorf("transfer %s failed; local backups retained in %s: %w", name, o.OutputDir, err)
			}
		}
		count, err := h.transferAppBackup(file, sum, name, keep, o)
		if err != nil {
			if file == output && len(pending) == 0 {
				return fmt.Errorf("transfer %s failed; local backup retained at %s: %w", name, output, err)
			}
			return fmt.Errorf("transfer %s failed; local backups retained in %s: %w", name, o.OutputDir, err)
		}
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("remote backup verified; local cleanup failed at %s: %w", file, err)
		}
		removed += count
		if file != output {
			h.say("Transferred earlier local backup %s left by a failed transfer; verified SHA256; local archive removed", filepath.Base(file))
		}
	}
	h.say("Backed up %s to %s:%s; verified SHA256; local archive removed", name, o.Remote, path.Join(o.RemoteDir, filepath.Base(output)))
	if removed > 0 {
		h.say("Deleted %d older backups of %s from the backup server; kept the newest %d", removed, name, keep)
	}
	return nil
}

// pendingBackups lists app's other local archives, oldest first. The directory
// was checked as private and root-owned under the host lock.
func (h Host) pendingBackups(directory, app, current string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && entry.Name() != current {
			names = append(names, entry.Name())
		}
	}
	files := backup.Archives(names, app)
	for i, name := range files {
		files[i] = filepath.Join(directory, name)
	}
	return files, nil
}

func fileChecksum(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// stageSharedBackup copies the small server-wide recovery files every app
// archive carries and returns the saved server settings.
func (h Host) stageSharedBackup(shared string) (backup.Settings, error) {
	settings, err := h.backupSettings()
	if err != nil {
		return settings, err
	}
	for _, name := range []string{"git/id_ed25519", "git/id_ed25519.pub", "git/known_hosts", "composer/auth.json", "mysql-admin.json"} {
		if err := copyBackupFile(h.path(filepath.Join(h.Manager.StateDir, name)), filepath.Join(shared, "state", name), name != "git/id_ed25519" && name != "git/known_hosts"); err != nil {
			return settings, err
		}
	}
	if err := h.stageBackupDestination(filepath.Join(shared, "state")); err != nil {
		return settings, err
	}
	if err := h.copyBackupTemplates(filepath.Join(shared, "templates")); err != nil {
		return settings, err
	}
	for name, source := range sharedBackupFiles() {
		if err := copyBackupFile(h.path(source), filepath.Join(shared, name), true); err != nil {
			return settings, err
		}
	}
	return settings, nil
}

// pruneLocalBackups deletes app's local archives beyond the newest keep. The
// directory was checked as private and root-owned under the host lock.
func (h Host) pruneLocalBackups(directory, app string, keep int, protect string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	for _, name := range backup.Expired(names, app, keep, protect) {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
		h.say("Deleted older local backup %s", name)
	}
	return nil
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

// captureAppBackup publishes output and returns its SHA256, hashed while writing.
// captureAppBackup publishes output and returns its SHA256, hashed while writing.
func (h Host) captureAppBackup(a config.App, c config.Config, r ports.Registry, settings backup.Settings, shared, output string) (string, error) {
	c.Apps = []config.App{a}
	appPorts := ports.Empty()
	for _, assignment := range r.Assignments {
		if assignment.App == a.Name {
			appPorts.Assignments = append(appPorts.Assignments, assignment)
		}
	}
	records, err := h.backupRepositories(c)
	if err != nil {
		return "", err
	}
	var creds *credentials
	_, err = os.Lstat(h.path(h.credentialsPath(a.Name)))
	if os.IsNotExist(err) && a.Type == "laravel" {
		return "", fmt.Errorf("full app backup requires recorded managed MySQL credentials")
	}
	if !os.IsNotExist(err) || a.Database.Enabled {
		dbApp := a
		dbApp.Database.Enabled = true
		record, err := h.transferCredentials(dbApp)
		if err != nil {
			return "", err
		}
		creds = &record
	}
	if a.Type == "laravel" {
		if _, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env"))); err != nil {
			return "", err
		}
	}
	stage, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".app-backup-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := backup.CopyTree(shared, stage); err != nil {
		return "", err
	}
	if err := h.captureBackupCode(a, records[0], stage); err != nil {
		return "", err
	}
	if creds != nil {
		if err := os.MkdirAll(filepath.Join(stage, "sql"), 0700); err != nil {
			return "", err
		}
		if err := h.dumpDatabase(*creds, filepath.Join(stage, "sql", a.Name+".sql")); err != nil {
			return "", err
		}
		data, err := json.Marshal(creds)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(stage, "state/databases"), 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(stage, "state/databases", a.Name+".json"), data, 0600); err != nil {
			return "", err
		}
	}
	if err := copyBackupFile(h.path(filepath.Join(a.Directory, ".env")), filepath.Join(stage, "apps", a.Name, ".env"), a.Type == "nuxt"); err != nil {
		return "", err
	}
	if err := copyBackupFile(h.path(h.backupRecordPath(a.Name)), filepath.Join(stage, "state/backups", a.Name+".json"), true); err != nil {
		return "", err
	}
	live := map[string]*os.Root{}
	defer closeLiveTrees(live)
	if a.Type == "laravel" {
		if err := stageLiveTree(live, stage, "apps/"+a.Name+"/storage/app", h.path(a.Directory), "storage/app"); err != nil {
			return "", err
		}
	}
	m := backup.Manifest{Version: 1, Scope: "app", CreatedAt: time.Now().UTC(), Config: c, Ports: appPorts, Apps: records, Settings: settings}
	f, err := os.CreateTemp(filepath.Dir(output), ".abr-app-backup-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	if err := backup.Write(io.MultiWriter(f, hash), stage, &m, live); err != nil {
		return "", err
	}
	if err := validateFullBackup(m, stage); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), output); err != nil {
		return "", err
	}
	dir, err := os.Open(filepath.Dir(output))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// The receiver publishes by hard link only after hashing and syncing the upload.
// SSH's exit status alone is insufficient: an exact receipt is required too.
// It then deletes this app's archives beyond the newest keep (0 keeps all),
// matching only exact NAME-YYYYMMDDHHMMSS.tar.gz names like backup.Expired,
// and uploads interrupted over a day ago. The backup server needs no setup.
const backupReceiver = `set -eu
umask 077
export LC_ALL=C
directory=$1
name=$2
expected=$3
keep=$4
app=$5
case $keep in ''|*[!0-9]*) exit 1 ;; esac
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
find . -maxdepth 1 -type f -name '.abr-upload-*' -mmin +1440 -delete 2>/dev/null || true
removed=0
if [ "$keep" -gt 0 ]; then
	digits='[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]'
	count=0
	for archive in "$app"-$digits.tar.gz; do
		if [ -f "$archive" ] && [ ! -L "$archive" ]; then count=$((count + 1)); fi
	done
	excess=$((count - keep))
	for archive in "$app"-$digits.tar.gz; do
		[ "$excess" -gt 0 ] || break
		if [ -f "$archive" ] && [ ! -L "$archive" ]; then
			excess=$((excess - 1))
			if [ "$archive" != "$name" ]; then
				rm -f -- "$archive"
				removed=$((removed + 1))
			fi
		fi
	done
	sync -f .
fi
printf 'ABR_BACKUP_OK %s %s %s\n' "$actual" "$name" "$removed"
`

// transferAppBackup uploads and verifies one archive, then lets the receiver
// apply retention; it returns how many older archives the receiver deleted.
func (h Host) transferAppBackup(file, checksum, app string, keep int, o BackupOptions) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	name := filepath.Base(file)
	command := "sh -c " + shellQuote(backupReceiver) + " abr-backup " + shellQuote(o.RemoteDir) + " " + shellQuote(name) + " " + shellQuote(checksum) + " " + strconv.Itoa(keep) + " " + shellQuote(app)
	out, err := h.backupSSH(o, command, f)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 4 || fields[0] != "ABR_BACKUP_OK" || fields[1] != checksum || fields[2] != name {
		return 0, fmt.Errorf("remote verification receipt missing or invalid")
	}
	removed, err := strconv.Atoi(fields[3])
	if err != nil || removed < 0 {
		return 0, fmt.Errorf("remote verification receipt missing or invalid")
	}
	return removed, nil
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
	var diagnostics commandOutput
	c := Command{Name: "ssh", Args: append(args, "--", o.Remote, command), Stdin: input, Stderr: &diagnostics, Private: true}
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
	out, err := h.run("Transfer or verify private backup destination", c)
	if err != nil {
		return out, backupSSHError(o, diagnostics.Bytes(), err)
	}
	return out, nil
}
