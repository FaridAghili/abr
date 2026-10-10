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
  abr_ci restore "${app_restore_archives[@]}" --dry-run
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
  # Reset only the recorded disposable fixtures for the following full archive
  # test. Retain the shared runtime ownership record just as its first reset did.
  for app_restore_name in fixture-php fixture-octane fixture-ssr fixture-spa; do
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
  echo 'Multiple app archives restored saved source, storage, SQL and HTTPS on a fresh target.'
)
