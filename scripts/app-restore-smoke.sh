#!/usr/bin/env bash
# Sourced at the fresh-target point of the disposable full restore test.
(
  if [[ ${GITHUB_ACTIONS:-false} != true && ${ABR_HOST_TEST:-0} != 1 ]]; then
    echo 'Refusing app restore test outside a disposable Ubuntu host.' >&2; exit 1
  fi
  set -euo pipefail
  mapfile -t app_restore_archives < <(sudo find /var/backups/abr-ci-app-remote -maxdepth 1 -name '*.tar.gz' | sort)
  test "${#app_restore_archives[@]}" = 4
  # Validate the combined archives only after the disposable target is fresh.
  # A directory selects the newest backup of each app.
  abr_ci restore /var/backups/abr-ci-app-remote --dry-run
  sudo test ! -e /var/lib/abr-ci/setup.json
  sudo test ! -e /etc/abr-ci/config.toml
  for app_restore_name in fixture-php fixture-octane fixture-ssr fixture-spa; do
    sudo test ! -e "/srv/apps/$app_restore_name"
  done
  abr_ci restore "${app_restore_archives[@]}" --yes --admin-user root --ssh-port 22
  sudo cmp /srv/apps/fixture-php/.env "$abr_binary_directory/full-env-before"
  sudo cmp /var/lib/abr-ci/databases/fixture-php.json "$abr_binary_directory/full-db-before"
  sudo grep -Fx 'saved custom storage' /srv/apps/fixture-php/storage/app/custom/data.txt
  sudo grep -Fx 'saved local configuration' /srv/apps/fixture-php/full-local.ini
  sudo grep -Fx 'saved private upload' /srv/apps/fixture-php/storage/app/private/full-private.txt
  sudo test ! -e /srv/apps/fixture-php/public/full-latest.txt
  sudo runuser -u abr-fixture-php -- /usr/bin/git -C /srv/apps/fixture-php rev-parse HEAD > "$abr_binary_directory/app-head-after"
  cmp "$abr_binary_directory/full-head-before" "$abr_binary_directory/app-head-after"
  sudo mysql --protocol=socket --user=root --batch --skip-column-names fixture_php <<'SQL' | grep -Fx 73
SELECT id FROM abr_full_backup_probe;
SQL
  test -z "$(sudo redis-cli -n 5 GET abr-full-state)"
  fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
  curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/full-saved.txt | grep -F 'saved working source'
  fixture_https api.fixture-octane.localhost | grep -F 'Abr Nuxt fixture'
  if sudo systemctl is-active --quiet abr-fixture-spa-nuxt.service; then
    echo 'App restore enabled a previously disabled app' >&2; exit 1
  fi
  # A set-up server adds one app backup as a new app. Purge only fixture-php,
  # then restore it while the other restored apps keep their data and services.
  abr_ci remove fixture-php --purge --yes
  app_restore_php=$(printf '%s\n' "${app_restore_archives[@]}" | grep -F '/fixture-php-')
  abr_ci restore "$app_restore_php" --dry-run > "$abr_binary_directory/app-into-server.log"
  grep -F 'into this server' "$abr_binary_directory/app-into-server.log"
  sudo test ! -e /srv/apps/fixture-php
  abr_ci restore "$app_restore_php" --yes
  sudo cmp /srv/apps/fixture-php/.env "$abr_binary_directory/full-env-before"
  sudo grep -Fx 'saved private upload' /srv/apps/fixture-php/storage/app/private/full-private.txt
  sudo mysql --protocol=socket --user=root --batch --skip-column-names fixture_php <<'SQL' | grep -Fx 73
SELECT id FROM abr_full_backup_probe;
SQL
  fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
  fixture_https api.fixture-octane.localhost | grep -F 'Abr Nuxt fixture'
  fixture_refused 'already registered on this server' restore "$app_restore_php" --yes
  # The same backup restored under a new name beside the registered app. Move
  # the existing app off every saved domain first; renamed restores keep them.
  sudo cp /var/lib/abr-ci/databases/fixture-php.json "$abr_binary_directory/app-existing-db"
  abr_ci edit fixture-php --domain fixture-php-current.localhost --alias '' --serving-domain ''
  abr_ci enable fixture-php
  abr_ci restore "$app_restore_php" --name fixture-php-old --dry-run > "$abr_binary_directory/app-renamed.log"
  grep -F 'Restoring fixture-php as fixture-php-old' "$abr_binary_directory/app-renamed.log"
  sudo test ! -e /srv/apps/fixture-php-old
  abr_ci restore "$app_restore_php" --name fixture-php-old --yes
  abr_ci list | grep -F fixture-php-old
  sudo grep -Fx 'DB_DATABASE=fixture_php_old' /srv/apps/fixture-php-old/.env
  sudo grep -Fx 'DB_USERNAME=fixture_php_old' /srv/apps/fixture-php-old/.env
  sudo grep -Fx 'saved private upload' /srv/apps/fixture-php-old/storage/app/private/full-private.txt
  test "$(sudo stat -c %U /srv/apps/fixture-php-old/.env)" = abr-fixture-php-old
  getent passwd abr-fixture-php-old | grep -F ':abr-fixture-php-old:/var/lib/abr-users/abr-fixture-php-old:'
  sudo test -e /etc/caddy/abr.d/abr-fixture-php-old.caddy
  sudo test -e /etc/php/8.5/fpm/pool.d/abr-fixture-php-old.conf
  sudo test -e /var/lib/abr-ci/env/fixture-php-old.env
  if sudo grep -E 'abr-fixture-php([^-]|$)|/srv/apps/fixture-php([^-]|$)' /etc/caddy/abr.d/abr-fixture-php-old.caddy /etc/php/8.5/fpm/pool.d/abr-fixture-php-old.conf /var/lib/abr-ci/env/fixture-php-old.env; then
    echo 'Renamed app services still name the original app' >&2; exit 1
  fi
  sudo mysql --protocol=socket --user=root --batch --skip-column-names fixture_php_old <<'SQL' | grep -Fx 73
SELECT id FROM abr_full_backup_probe;
SQL
  sudo mysql --protocol=socket --user=root --batch --skip-column-names <<'SQL' > "$abr_binary_directory/app-renamed-grants.log"
SHOW GRANTS FOR 'fixture_php_old'@'localhost';
SHOW GRANTS FOR 'fixture_php_old'@'127.0.0.1';
SQL
  if grep -E '`fixture\\?_php`' "$abr_binary_directory/app-renamed-grants.log"; then
    echo 'Renamed MySQL account can reach the original database' >&2; exit 1
  fi
  grep -E '`fixture\\?_php\\?_old`' "$abr_binary_directory/app-renamed-grants.log"
  # The existing app keeps its project, secrets, database record and service.
  sudo cmp /srv/apps/fixture-php/.env "$abr_binary_directory/full-env-before"
  sudo cmp /var/lib/abr-ci/databases/fixture-php.json "$abr_binary_directory/app-existing-db"
  fixture_https fixture-php-current.localhost | grep -F 'Laravel fixture database=1'
  fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
  fixture_https extra.fixture-php.localhost | grep -F 'Laravel fixture database=1'
  # Reset only the recorded disposable fixtures for the following full archive
  # test. Retain the shared runtime ownership record just as its first reset did.
  for app_restore_name in fixture-php-old fixture-php fixture-octane fixture-ssr fixture-spa; do
    abr_ci remove "$app_restore_name" --purge --yes
  done
  sudo mv /var/lib/abr-ci "$abr_binary_directory/app-restored-state"
  sudo install -d -m 700 /var/lib/abr-ci
  sudo mv "$abr_binary_directory/app-restored-state/node-tools.json" /var/lib/abr-ci/node-tools.json
  sudo mv /etc/abr-ci/config.toml "$abr_binary_directory/app-restored-config.toml"
  sudo mysql --protocol=socket --user=root <<'SQL'
DROP USER 'root'@'127.0.0.1';
SQL
  sudo systemctl stop caddy
  sudo rm -rf /var/lib/caddy/.local
  echo 'Multiple app archives restored on a fresh target, and one app backup restored into the set-up server.'
)
