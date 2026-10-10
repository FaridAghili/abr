package host

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Return fixed failure codes and numeric metadata, never arbitrary remote output.
const backupDestinationCheck = `set -eu
fail() { printf 'ABR_DESTINATION_ERROR %s\n' "$*"; exit 1; }
directory=$1
parent=$directory
while [ "$parent" != / ]; do
    parent=${parent%/*}
    [ -n "$parent" ] || parent=/
    if [ -d "$parent" ] && [ ! -x "$parent" ]; then fail parent-access; fi
    if [ -e "$parent" ] && [ ! -d "$parent" ]; then fail parent-not-directory; fi
done
if [ ! -d "$directory" ]; then
    if [ -e "$directory" ] || [ -L "$directory" ]; then fail not-directory; fi
    fail missing
fi
resolved=$(realpath -e -- "$directory") || fail resolve
[ "$resolved" = "$directory" ] || fail symlink
owner=$(stat -c '%u' -- "$directory") || fail stat
account=$(id -u) || fail account
[ "$owner" = "$account" ] || fail "owner $owner $account"
mode=$(stat -c '%a' -- "$directory") || fail stat
[ "$mode" = 700 ] || fail "mode $mode"
[ -w "$directory" ] && [ -x "$directory" ] || fail access
probe=$(mktemp "$directory/.abr-destination-check-XXXXXXXXXX") || fail write
trap 'rm -f -- "$probe"' EXIT HUP INT TERM
printf 'abr destination check\n' > "$probe" || fail write
rm -f -- "$probe" || fail cleanup
trap - EXIT HUP INT TERM
printf 'ABR_DESTINATION_OK\n'
`

func backupDestinationError(o BackupOptions, output []byte) error {
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) < 2 || fields[0] != "ABR_DESTINATION_ERROR" {
		return nil
	}
	prefix := fmt.Sprintf("backup directory %q on %s", o.RemoteDir, o.Remote)
	if len(fields) == 2 {
		switch fields[1] {
		case "missing":
			return fmt.Errorf("%s does not exist; on the backup server, run: mkdir -p %s && chmod 700 %s", prefix, shellQuote(o.RemoteDir), shellQuote(o.RemoteDir))
		case "not-directory":
			return fmt.Errorf("%s is not a directory; configure a directory for backup archives", prefix)
		case "parent-not-directory":
			return fmt.Errorf("%s has a parent path that is not a directory; correct the configured backup path", prefix)
		case "parent-access":
			return fmt.Errorf("%s cannot be reached because the SSH user lacks execute permission on a parent directory; allow this account to traverse the parent directories", prefix)
		case "symlink":
			return fmt.Errorf("%s resolves through a symlink; configure its real absolute path", prefix)
		case "resolve", "stat":
			return fmt.Errorf("%s could not be inspected; check directory access and that realpath and stat are available on the backup server", prefix)
		case "account":
			return fmt.Errorf("could not determine the SSH account's UID on %s; check that id is available on the backup server", o.Remote)
		case "access":
			return fmt.Errorf("%s cannot be written or accessed by the SSH user; check directory ACLs and filesystem permissions", prefix)
		case "write":
			return fmt.Errorf("%s could not create or write a test file; check available disk space, quota, a read-only filesystem and directory ACLs", prefix)
		case "cleanup":
			return fmt.Errorf("%s could not remove the temporary test file; check directory ACLs and filesystem permissions", prefix)
		}
	}
	if len(fields) == 3 && fields[1] == "mode" {
		if mode, err := strconv.ParseUint(fields[2], 8, 12); err == nil {
			return fmt.Errorf("%s has mode %04o; required mode is 0700. On the backup server, run: chmod 700 %s", prefix, mode, shellQuote(o.RemoteDir))
		}
	}
	if len(fields) == 4 && fields[1] == "owner" {
		owner, err1 := strconv.ParseUint(fields[2], 10, 32)
		account, err2 := strconv.ParseUint(fields[3], 10, 32)
		if err1 == nil && err2 == nil {
			user, _, _ := strings.Cut(o.Remote, "@")
			return fmt.Errorf("%s is owned by UID %d; SSH user %s has UID %d. On the backup server, run as root: chown %s %s", prefix, owner, user, account, shellQuote(user), shellQuote(o.RemoteDir))
		}
	}
	return fmt.Errorf("backup server returned an invalid directory failure result; check the SSH account's shell and login scripts")
}

// SSH can include server banners or private data on stderr. Recognize causes,
// but show only our own messages; never copy raw diagnostics into logs/errors.
func backupSSHError(o BackupOptions, diagnostics []byte, cause error) error {
	text := string(diagnostics)
	port := o.SSHPort
	if port == 0 {
		port = 22
	}
	target := fmt.Sprintf("%s (SSH port %d)", o.Remote, port)
	var detail string
	switch {
	case strings.Contains(text, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		detail = "SSH host key has changed; existing trust was preserved. Verify the server's fingerprint from its console before updating known_hosts"
	case strings.Contains(text, "Host key verification failed"):
		detail = "SSH host key is not trusted or could not be verified. Use Test backup destination in the TUI, or abr backup host-key and abr backup trust --fingerprint SHA256:... after checking the server's console"
	case strings.Contains(text, "Identity file ") && strings.Contains(text, "not accessible"), strings.Contains(text, "Load key "):
		detail = "SSH private key could not be loaded; configure an accessible, unencrypted private key with private file permissions"
	case strings.Contains(text, "Permission denied ("):
		if o.Password != "" {
			detail = "SSH password authentication failed; check the saved password and that the backup server allows password authentication for this account"
		} else {
			detail = "SSH key authentication failed; add Abr's backup public key to this account's ~/.ssh/authorized_keys on the backup server (.ssh mode 700, authorized_keys mode 600) and check the configured username"
		}
	case strings.Contains(text, "Connection refused"):
		detail = "SSH connection was refused; check that sshd is running and listening on the configured port and that the firewall allows it"
	case strings.Contains(text, "Connection timed out"), strings.Contains(text, "Operation timed out"):
		detail = "SSH connection timed out; check the server address, SSH port, firewall and network reachability"
	case strings.Contains(text, "No route to host"), strings.Contains(text, "Network is unreachable"):
		detail = "backup server is unreachable; check routing, network connectivity and firewall rules"
	case strings.Contains(text, "Could not resolve hostname"), strings.Contains(text, "getaddrinfo "):
		detail = "backup hostname could not be resolved; check the configured hostname and DNS"
	case strings.Contains(text, "Connection reset"), strings.Contains(text, "Connection closed"), strings.Contains(text, "Broken pipe"):
		detail = "SSH connection was closed unexpectedly; check sshd logs on the backup server and network connectivity"
	case strings.Contains(text, "No space left on device"):
		detail = "backup filesystem is out of space; free space on the backup server"
	case strings.Contains(text, "Disk quota exceeded"):
		detail = "backup account's disk quota is exceeded; free space or increase its quota"
	case strings.Contains(text, "Read-only file system"):
		detail = "backup filesystem is read-only; use a writable backup directory or correct the filesystem mount"
	}
	var exit *exec.ExitError
	if detail == "" && o.Password != "" && errors.As(cause, &exit) && exit.ExitCode() == 5 {
		detail = "SSH password authentication failed; check the saved password and that the backup server allows password authentication for this account"
	}
	if detail != "" {
		return fmt.Errorf("backup destination %s: %s: %w", target, detail, cause)
	}
	return fmt.Errorf("backup destination %s: SSH command failed; check sshd logs on the backup server for the cause: %w", target, cause)
}
