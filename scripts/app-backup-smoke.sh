#!/usr/bin/env bash
# Sourced only by the disposable-host full-transfer smoke test.
(
  if [[ ${GITHUB_ACTIONS:-false} != true && ${ABR_HOST_TEST:-0} != 1 ]]; then
    echo 'Refusing app backup test outside a disposable Ubuntu host.' >&2; exit 1
  fi
  set -euo pipefail
  app_backup_remote=/var/backups/abr-ci-app-remote
  app_backup_local=/var/backups/abr-ci-app-local
  app_backup_password_remote=/var/backups/abr-ci-password-remote
  app_backup_sshd_pid=''
  trap 'if [[ -n $app_backup_sshd_pid ]]; then sudo kill "$app_backup_sshd_pid" || true; fi; sudo userdel abr-backup-fixture >/dev/null 2>&1 || true' EXIT
  sudo install -d -m 700 "$app_backup_remote"
  # Pin the independently known disposable host fingerprint through Abr;
  # no manual SSH login or direct known_hosts edit is needed.
  app_backup_fingerprint=$(sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print $2}')
  abr_ci backup configure --host 127.0.0.1 --user root --path "$app_backup_remote" --key /root/.ssh/abr-fixture-ssh
  abr_ci backup host-key
  fixture_refused 'does not match the confirmed fingerprint' backup trust --fingerprint SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  abr_ci backup trust --fingerprint "$app_backup_fingerprint"
  sudo cp /root/.ssh/known_hosts "$abr_binary_directory/backup-known-hosts-before"
  abr_ci backup trust --fingerprint "$app_backup_fingerprint"
  sudo cmp /root/.ssh/known_hosts "$abr_binary_directory/backup-known-hosts-before"
  abr_ci backup test
  sudo systemctl show caddy php8.5-fpm redis-server abr-fixture-php-queue@1.service \
    --property=Id,MainPID,ActiveState > "$abr_binary_directory/app-services-before"
  abr_ci backup --all --transfer --output-dir "$app_backup_local"
  sudo systemctl show caddy php8.5-fpm redis-server abr-fixture-php-queue@1.service \
    --property=Id,MainPID,ActiveState > "$abr_binary_directory/app-services-after"
  cmp "$abr_binary_directory/app-services-before" "$abr_binary_directory/app-services-after"
  sudo python3 - "$app_backup_local" "$app_backup_remote" <<'PY'
import json, re, sys, tarfile
from pathlib import Path
local, remote = map(Path, sys.argv[1:])
assert not list(local.glob('*.tar.gz'))
archives = sorted(remote.glob('*.tar.gz'))
assert len(archives) == 4
for archive in archives:
    assert archive.stat().st_mode & 0o777 == 0o600
    with tarfile.open(archive) as tar:
        m = json.load(tar.extractfile('manifest.json'))
        app = m['apps'][0]['name']
        assert re.fullmatch(re.escape(app) + r'-[0-9]{14}\.tar\.gz', archive.name)
        assert m['scope'] == 'app'
        assert len(m['apps']) == 1
        assert len(m['apps'][0]['commit']) == 40
        assert tar.getmember(f'apps/{app}/repository.bundle').size > 0
        assert tar.getmember(f'apps/{app}/source').isdir()
        if app == 'fixture-php':
            assert tar.extractfile(f'apps/{app}/source/full-local.ini').read() == b'saved local configuration\n'
            assert tar.extractfile(f'apps/{app}/storage/app/custom/data.txt').read() == b'saved custom storage\n'
        assert not any(n.startswith(('redis/', 'caddy/')) for n in tar.getnames())
PY
  # Empty abr state alone is not a fresh target: existing projects must still
  # prevent a restore preview, without creating state or changing the apps.
  mapfile -t app_backup_archives < <(sudo find "$app_backup_remote" -maxdepth 1 -name '*.tar.gz' | sort)
  if sudo "$abr_test_binary" --config "$abr_binary_directory/app-restore/config.toml" \
    --state-dir "$abr_binary_directory/app-restore/state" restore "${app_backup_archives[@]}" --dry-run \
    > "$abr_binary_directory/app-restore-refusal.log" 2>&1; then
    echo 'Restore preview accepted existing projects' >&2; exit 1
  fi
  grep -F 'restore target already exists: /srv/apps/' "$abr_binary_directory/app-restore-refusal.log"
  sudo test ! -e "$abr_binary_directory/app-restore/state"
  # Fail on a nonexistent remote directory, retain the first local archive and
  # never advance to the second selected app.
  fixture_refused 'local backup retained' backup fixture-php fixture-octane \
    --remote root@127.0.0.1 --remote-dir /var/backups/abr-ci-missing-destination --output-dir "$app_backup_local"
  sudo python3 - "$app_backup_local" <<'PY'
from pathlib import Path
import sys
files = list(Path(sys.argv[1]).glob('*.tar.gz'))
assert len(files) == 1 and files[0].name.startswith('fixture-php-')
PY
  # Exercise real password auth on an isolated loopback SSH daemon. The main
  # SSH service retains its key-only authentication settings.
  sudo useradd --no-create-home --shell /bin/sh abr-backup-fixture
  printf 'abr-backup-fixture:ci-backup-password\n' | sudo chpasswd
  sudo install -d -m 700 -o abr-backup-fixture -g abr-backup-fixture "$app_backup_password_remote"
  app_backup_port=$(python3 - <<'PY'
import socket
with socket.socket() as s:
    s.bind(('127.0.0.1', 0))
    print(s.getsockname()[1])
PY
)
  cat > "$abr_binary_directory/backup-sshd.conf" <<EOF
Port $app_backup_port
ListenAddress 127.0.0.1
HostKey /etc/ssh/ssh_host_ed25519_key
PidFile $abr_binary_directory/backup-sshd.pid
PasswordAuthentication yes
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin no
AllowUsers abr-backup-fixture
EOF
  sudo /usr/sbin/sshd -f "$abr_binary_directory/backup-sshd.conf" -E "$abr_binary_directory/backup-sshd.log"
  app_backup_sshd_pid=$(sudo cat "$abr_binary_directory/backup-sshd.pid")
  printf 'ci-backup-password\n' | abr_ci backup configure --host 127.0.0.1 --user abr-backup-fixture \
    --path "$app_backup_password_remote" --ssh-port "$app_backup_port" --password-stdin
  abr_ci backup host-key
  abr_ci backup trust --fingerprint "$app_backup_fingerprint"
  abr_ci backup test
  abr_ci backup fixture-octane --transfer --output-dir "$app_backup_local"
  sudo test "$(sudo find "$app_backup_password_remote" -maxdepth 1 -name '*.tar.gz' | wc -l)" = 1
  # Clear only this test's destination so the following full-server round trip
  # continues without depending on the temporary SSH daemon.
  sudo rm /var/lib/abr-ci/backup-destination.json
  echo 'Online app backup, verified SSH transfer, password auth and failure retention passed.'
)
