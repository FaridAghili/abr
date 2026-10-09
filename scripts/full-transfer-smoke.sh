#!/usr/bin/env bash
# Sourced by host-smoke.sh only on its disposable host. No external Git traffic.
if [[ ${GITHUB_ACTIONS:-false} != true && ${ABR_HOST_TEST:-0} != 1 ]]; then
 echo "Refusing full transfer test outside a disposable Ubuntu host." >&2; exit 1
fi
set -euo pipefail
full_backup_fixture=/var/backups/abr-ci-full.tar.gz
full_git_fixture=/srv/abr-full-git
sudo install -d -m 755 "$full_git_fixture"
full_apps=(fixture-php fixture-octane fixture-ssr fixture-spa)
for full_app in "${full_apps[@]}"; do
  full_directory=/srv/apps/$full_app
  sudo runuser -u "abr-$full_app" -- /usr/bin/git -C "$full_directory" remote add origin "git@github.com:fixture/$full_app.git"
  sudo runuser -u "abr-$full_app" -- /usr/bin/git -C "$full_directory" bundle create "$full_directory/.git/full-fixture.bundle" --all
  sudo /usr/bin/git clone --bare "$full_directory/.git/full-fixture.bundle" "$full_git_fixture/$full_app.git"
  sudo rm "$full_directory/.git/full-fixture.bundle"
  sudo chown -hR _apt "$full_git_fixture/$full_app.git"
  sudo chmod -R a+rX "$full_git_fixture/$full_app.git"
done
# Clone calls still execute Git as _apt with no_new_privs, using local fixtures.
test ! -e /usr/local/bin/git
cat > "$abr_binary_directory/full-git-shim" <<'SH'
#!/bin/sh
exec /usr/bin/git -c url.file:///srv/abr-full-git/.insteadOf=git@github.com:fixture/ "$@"
SH
sudo install -m 755 "$abr_binary_directory/full-git-shim" /usr/local/bin/git
fixture_git_shim=1

sudo runuser -u abr-fixture-php -- mkdir -p /srv/apps/fixture-php/storage/app/private
printf '%s' 'saved public upload' | sudo runuser -u abr-fixture-php -- tee /srv/apps/fixture-php/storage/app/public/full-public.txt >/dev/null
printf '%s' 'saved private upload' | sudo runuser -u abr-fixture-php -- tee /srv/apps/fixture-php/storage/app/private/full-private.txt >/dev/null
printf '%s\n' 'NUXT_FULL_BACKUP=preserved' | sudo runuser -u abr-fixture-spa -- tee /srv/apps/fixture-spa/.env >/dev/null
sudo runuser -u abr-fixture-spa -- chmod 600 /srv/apps/fixture-spa/.env
sudo cp /srv/apps/fixture-php/.env "$abr_binary_directory/full-env-before"
sudo cp /var/lib/abr-ci/databases/fixture-php.json "$abr_binary_directory/full-db-before"
sudo cp /var/lib/abr-ci/git/id_ed25519 "$abr_binary_directory/full-git-before"
abr_ci ports > "$abr_binary_directory/full-ports-before"
# A non-cache Redis DB proves session/queue state survives normal cache clearing.
sudo redis-cli -n 5 SET abr-full-state saved-redis-state >/dev/null
sudo mysql --protocol=socket --user=root fixture_php <<'SQL'
CREATE TABLE abr_full_backup_probe (id INT PRIMARY KEY);
INSERT INTO abr_full_backup_probe VALUES (73);
SQL
printf '\n; Full backup edited template fixture\n' | sudo tee -a /etc/abr/templates/php-cli.ini.tmpl >/dev/null
sudo cp /etc/abr/templates/php-cli.ini.tmpl "$abr_binary_directory/full-template-before"
# Disabled applications must remain disabled after a successful round trip.
abr_ci disable fixture-spa
abr_ci backup --output "$full_backup_fixture"
sudo test "$(sudo stat -c '%a' "$full_backup_fixture")" = 600
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
if sudo systemctl is-active --quiet abr-fixture-spa-nuxt.service; then
 echo "Backup enabled a previously disabled app" >&2; exit 1
fi
sudo tar -tzf "$full_backup_fixture" > "$abr_binary_directory/full-inventory"
if grep -E '(^|/)(vendor|node_modules|\.git|\.output)/' "$abr_binary_directory/full-inventory"; then
  echo 'Full backup included rebuildable project data' >&2; exit 1
fi
# Restore must use code newer than the backup, rather than the captured revision.
full_worktree="$abr_binary_directory/full-latest-source"
sudo /usr/bin/git -c safe.directory="$full_git_fixture/fixture-php.git" clone "$full_git_fixture/fixture-php.git" "$full_worktree"
sudo /usr/bin/git -C "$full_worktree" config user.name 'Abr backup fixture'
sudo /usr/bin/git -C "$full_worktree" config user.email 'fixture@example.invalid'
printf '%s\n' 'latest restored Git code' | sudo tee "$full_worktree/public/full-latest.txt" >/dev/null
sudo /usr/bin/git -C "$full_worktree" add public/full-latest.txt
sudo /usr/bin/git -C "$full_worktree" commit --no-gpg-sign -m 'Latest code after backup'
sudo /usr/bin/git -C "$full_worktree" -c safe.directory="$full_git_fixture/fixture-php.git" push origin main

fixture_refused 'pass --yes' restore "$full_backup_fixture"
fixture_refused 'fresh server' restore "$full_backup_fixture" --yes
# Remove only disposable fixture apps and move their now-empty manager state.
for full_app in "${full_apps[@]}"; do abr_ci remove "$full_app" --purge --yes; done
sudo mv /var/lib/abr-ci "$abr_binary_directory/full-old-state"
sudo mv /etc/abr-ci/config.toml "$abr_binary_directory/full-old-config.toml"
sudo mysql --protocol=socket --user=root <<'SQL'
DROP USER 'root'@'127.0.0.1';
SQL
sudo redis-cli -n 5 DEL abr-full-state >/dev/null
sudo systemctl stop caddy
sudo rm -rf /var/lib/caddy/.local
abr_ci restore "$full_backup_fixture" --dry-run
sudo test ! -e /var/lib/abr-ci/setup.json
abr_ci restore "$full_backup_fixture" --yes --admin-user root --ssh-port 22
sudo cmp /srv/apps/fixture-php/.env "$abr_binary_directory/full-env-before"
sudo cmp /var/lib/abr-ci/databases/fixture-php.json "$abr_binary_directory/full-db-before"
sudo cmp /var/lib/abr-ci/git/id_ed25519 "$abr_binary_directory/full-git-before"
sudo cmp /etc/abr/templates/php-cli.ini.tmpl "$abr_binary_directory/full-template-before"
abr_ci ports > "$abr_binary_directory/full-ports-after"
cmp "$abr_binary_directory/full-ports-before" "$abr_binary_directory/full-ports-after"
sudo grep -Fx 'NUXT_FULL_BACKUP=preserved' /srv/apps/fixture-spa/.env
sudo test ! -e /srv/apps/fixture-ssr/.env
sudo grep -Fx 'saved public upload' /srv/apps/fixture-php/storage/app/public/full-public.txt
sudo grep -Fx 'saved private upload' /srv/apps/fixture-php/storage/app/private/full-private.txt
sudo test "$(sudo stat -c '%U:%a' /srv/apps/fixture-php/.env)" = abr-fixture-php:600
sudo test "$(sudo stat -c '%U' /srv/apps/fixture-php/storage/app/private/full-private.txt)" = abr-fixture-php
fixture_denied nobody cat /srv/apps/fixture-php/storage/app/private/full-private.txt
sudo mysql --protocol=socket --user=root --batch --skip-column-names fixture_php <<'SQL' | grep -Fx 73
SELECT id FROM abr_full_backup_probe;
SQL
sudo redis-cli -n 5 GET abr-full-state | grep -Fx saved-redis-state
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/full-latest.txt | grep -F 'latest restored Git code'
fixture_https www.fixture-octane.localhost | grep -F 'Laravel fixture database=1'
fixture_https api.fixture-octane.localhost | grep -F 'Abr Nuxt fixture'
# App manifests must reference new resources, and disabled Nuxt must stay stopped.
sudo python3 - <<'PY'
import json
from pathlib import Path
manifest = json.loads(Path('/var/lib/abr-ci/apps/fixture-php.json').read_text())
assert manifest['Enabled']
assert not Path('/var/lib/abr-ci/apps/fixture-spa.json').exists()
PY
abr_ci enable fixture-spa
fixture_https portal.fixture-php.localhost >/dev/null
abr_ci doctor
sudo rm /usr/local/bin/git
fixture_git_shim=0
sudo rm -rf "$full_git_fixture"
echo 'Full backup/latest-code restore round trip passed.'
