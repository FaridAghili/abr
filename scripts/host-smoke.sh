#!/usr/bin/env bash
# This changes an entire host. Run only in a disposable Ubuntu environment.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-false} != true && ${SITES_HOST_TEST:-0} != 1 ]]; then
  echo 'Refusing host test: use a disposable Ubuntu machine and SITES_HOST_TEST=1.' >&2
  exit 1
fi
sites_test_binary=$(realpath "${1:-bin/sites}")
sites_ci() {
  sudo "$sites_test_binary" --config /etc/sites-ci/config.toml --state-dir /var/lib/sites-ci "$@"
}
fixture_denied() {
  if sudo runuser -u "$1" -- "${@:2}" >/dev/null 2>&1; then
    echo "Expected access refusal for $1" >&2; exit 1
  fi
}
fixture_https() {
  # Local Caddy certificates can finish issuance shortly after configuration reload.
  curl --fail --silent --show-error --retry 10 --retry-all-errors --retry-delay 1 \
    --max-time 10 --insecure --resolve "$1:443:127.0.0.1" "https://$1/" "${@:2}"
}

# GitHub runner images ship MySQL with this documented test password and other
# inactive web servers. These adjustments belong only to the disposable fixture.
if [[ ${GITHUB_ACTIONS:-false} == true ]]; then
  sudo systemctl mask --now nginx apache2
  printf '[client]\nuser=root\npassword=root\n' | sudo tee /root/.my.cnf >/dev/null
  sudo chmod 600 /root/.my.cnf
fi
# A real key login prerequisite for the disposable host's root test account.
sudo apt-get install -y openssh-server
sudo install -d -m 700 /root/.ssh
sudo install -d -m 755 /run/sshd
sudo ssh-keygen -t ed25519 -N '' -f /root/.ssh/sites-fixture-ssh >/dev/null
sudo bash -c 'cat /root/.ssh/sites-fixture-ssh.pub >> /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys'
sudo apt-get install -y redis-server
sudo systemctl start redis-server
sudo redis-cli SET sites-fixture-persist survives-setup >/dev/null
sudo redis-cli SAVE >/dev/null
sites_ci setup --no-firewall --ssh-port 22 --admin-user root
sudo redis-cli GET sites-fixture-persist | grep -Fx survives-setup
# Caddy administration is restricted to root and Caddy, not application users.
sudo test -S /var/lib/caddy/sites-admin.sock
if curl --silent --max-time 2 http://127.0.0.1:2019/config/ >/dev/null; then
  echo 'Caddy administration is exposed on the default TCP port' >&2; exit 1
fi
/usr/local/bin/svgo --version
/usr/local/bin/ncu --version
/usr/local/bin/composer --no-plugins --no-scripts --version
sudo systemctl start ssh.service
# The host key exemption is only for this disposable localhost fixture.
sudo ssh -F /dev/null -i /root/.ssh/sites-fixture-ssh -o IdentitiesOnly=yes \
  -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=10 root@127.0.0.1 true
sudo /usr/sbin/sshd -T | grep -Fx 'passwordauthentication no'
sudo /usr/sbin/sshd -T | grep -Fx 'authenticationmethods publickey'
sudo mysql --protocol=socket --user=root --batch --skip-column-names \
  -e 'SELECT @@bind_address, @@local_infile;' | grep -Fx $'127.0.0.1\t0'
sudo redis-cli CONFIG GET bind | grep -Fx '127.0.0.1 -::1'
sudo redis-cli CONFIG GET appendonly | grep -Fx yes
sudo redis-cli CONFIG GET maxmemory-policy | grep -Fx noeviction
sudo mysql --protocol=socket --user=root --batch --skip-column-names \
  -e 'SELECT @@innodb_buffer_pool_size, @@max_connections;' | grep -Fx $'268435456\t100'
# Shared identity tests remain offline: no GitHub account or private repository.
sites_ci git setup
sites_ci git setup

fixture_source=$(mktemp -d)
trap 'rm -rf "$fixture_source"' EXIT
composer create-project --no-install --no-scripts --prefer-dist 'laravel/laravel:^13.0' "$fixture_source/laravel"
(
  cd "$fixture_source/laravel"
  composer require laravel/octane spiral/roadrunner-cli spiral/roadrunner-http --no-update --no-scripts --no-interaction
  composer update --no-install --no-scripts --no-interaction
  # npm 12's lock-only resolution incorrectly blocks bundled registry tarballs.
  # This opt-in is confined to generating our disposable fixture lockfile.
  npm install --package-lock-only --ignore-scripts --allow-remote=all
)
sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-php
sudo tee /srv/apps/fixture-php/routes/web.php >/dev/null <<'PHP'
<?php
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route;
Route::get('/', fn () => response('Laravel fixture database='.DB::select('SELECT 1 AS ok')[0]->ok)->header('X-Powered-By', 'fixture-runtime'));
Route::get('/php-config', fn () => response()->json(['expose_php' => ini_get('expose_php'), 'display_errors' => ini_get('display_errors'), 'opcache' => ini_get('opcache.enable')]));
PHP

fixture_git() {
  sudo git -C "$1" init --initial-branch=main
  sudo git -C "$1" config user.name 'Sites fixture'
  sudo git -C "$1" config user.email 'fixture@example.invalid'
  sudo git -C "$1" add .
  sudo git -C "$1" commit --no-gpg-sign -m 'Fixture'
}
fixture_git /srv/apps/fixture-php
sites_ci register --name fixture-php --dir /srv/apps/fixture-php --type laravel --domain fixture-php.localhost --scheduler
sudo bash -c 'awk "!/^DB_(CONNECTION|HOST|PORT|DATABASE|USERNAME|PASSWORD)=/" /srv/apps/fixture-php/.env.example > /srv/apps/fixture-php/.env; cat /var/lib/sites-ci/credentials/fixture-php.env >> /srv/apps/fixture-php/.env; chmod 600 /srv/apps/fixture-php/.env'
sites_ci deploy fixture-php --no-pull
fixture_denied sites-fixture-php curl --fail --silent --max-time 2 \
  --unix-socket /var/lib/caddy/sites-admin.sock http://localhost/config/
# Load the identity instead of test -r: Ubuntu's Rust test can ignore ACLs.
sudo runuser -u sites-fixture-php -- ssh-keygen -y -P '' -f /var/lib/sites-ci/git/id_ed25519 >/dev/null
sites_ci git setup
fixture_denied sites-fixture-php head -c 1 /var/lib/sites-ci/credentials/fixture-php.env
fixture_denied nobody head -c 1 /var/lib/sites-ci/git/id_ed25519
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
sudo test -S /run/php/sites-fixture-php.sock
sudo test "$(sudo stat -c '%a' /srv/apps/fixture-php/.env)" = 600
sudo test "$(sudo stat -c '%a' /var/lib/sites-ci/credentials/fixture-php.env)" = 600
# Private atomic SQL dump and an actual restore with the scoped database account.
sudo mysql --protocol=socket --user=root sites_fixture_php -e 'CREATE TABLE sites_backup_check (id INT PRIMARY KEY); INSERT INTO sites_backup_check VALUES (42);'
sites_ci database backup fixture-php --output-dir /var/lib/sites-ci/backups
sql_dump=$(sudo find /var/lib/sites-ci/backups -name 'fixture-php-*.sql' -print -quit)
sudo test "$(sudo stat -c '%a' "$sql_dump")" = 600
sites_ci disable fixture-php
sudo mysql --protocol=socket --user=root sites_fixture_php -e 'DROP TABLE sites_backup_check;'
sites_ci database import fixture-php "$sql_dump" --yes
sudo mysql --protocol=socket --user=root --batch --skip-column-names sites_fixture_php -e 'SELECT id FROM sites_backup_check;' | grep -Fx 42
# SQL import cannot escape to another database or execute a root shell command.
printf 'CREATE DATABASE forbidden_database;\n' | sudo tee /var/lib/sites-ci/forbidden.sql >/dev/null
if sites_ci database import fixture-php /var/lib/sites-ci/forbidden.sql --yes; then echo 'Unscoped SQL was allowed' >&2; exit 1; fi
printf '\\! touch /var/lib/sites-ci/import-shell-executed\n' | sudo tee /var/lib/sites-ci/shell.sql >/dev/null
if sites_ci database import fixture-php /var/lib/sites-ci/shell.sql --yes; then echo 'SQL client shell command was allowed' >&2; exit 1; fi
sudo test ! -e /var/lib/sites-ci/import-shell-executed
sites_ci enable fixture-php
fixture_https fixture-php.localhost --dump-header "$fixture_source/headers" >/dev/null
if grep -Ei '^(server|x-powered-by):' "$fixture_source/headers"; then echo 'Identifying response header leaked' >&2; exit 1; fi
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/php-config | grep -F '"opcache":"1"'
# A generated hashed asset is served with Brotli and immutable caching.
asset_path=$(sudo find /srv/apps/fixture-php/public/build/assets -name 'app-*.css' -print -quit)
sudo test -f "$asset_path.br"
asset_uri=${asset_path#/srv/apps/fixture-php/public}
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -H 'Accept-Encoding: br' --dump-header "$fixture_source/asset-headers" \
  "https://fixture-php.localhost$asset_uri" -o "$fixture_source/asset.br"
grep -Ei '^content-encoding: br' "$fixture_source/asset-headers"
grep -Fi 'Cache-Control: public, max-age=31536000, immutable' "$fixture_source/asset-headers"
brotli --decompress --stdout "$fixture_source/asset.br" | sudo cmp - "$asset_path"
for encoding in zstd gzip; do
  curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
    -H "Accept-Encoding: $encoding" --dump-header "$fixture_source/$encoding-headers" \
    "https://fixture-php.localhost$asset_uri" -o /dev/null
  grep -Ei "^content-encoding: $encoding" "$fixture_source/$encoding-headers"
done
curl --silent --resolve fixture-php.localhost:80:127.0.0.1 -D "$fixture_source/http-headers" http://fixture-php.localhost/ -o /dev/null
if grep -Ei '^server:' "$fixture_source/http-headers"; then echo 'HTTP redirect header leaked' >&2; exit 1; fi
sites_ci restart fixture-php web
sites_ci disable fixture-php
sites_ci enable fixture-php
sites_ci status fixture-php
sites_ci logs fixture-php scheduler

sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-octane
sudo cp /srv/apps/fixture-php/routes/web.php /srv/apps/fixture-octane/routes/web.php
printf '\n.rr.yaml\n' | sudo tee -a /srv/apps/fixture-octane/.gitignore >/dev/null
fixture_git /srv/apps/fixture-octane
sites_ci register --name fixture-octane --dir /srv/apps/fixture-octane --type laravel --web-driver octane --domain fixture-octane.localhost
sudo bash -c 'awk "!/^DB_(CONNECTION|HOST|PORT|DATABASE|USERNAME|PASSWORD)=/" /srv/apps/fixture-octane/.env.example > /srv/apps/fixture-octane/.env; cat /var/lib/sites-ci/credentials/fixture-octane.env >> /srv/apps/fixture-octane/.env; chmod 600 /srv/apps/fixture-octane/.env'
sites_ci deploy fixture-octane --no-pull
fixture_https fixture-octane.localhost | grep -F 'Laravel fixture database=1'
sudo test ! -f /srv/apps/fixture-octane/rr
sudo test -x /usr/local/bin/rr
sites_ci restart fixture-octane web
sites_ci database backup fixture-php fixture-octane --output-dir /var/lib/sites-ci/selected-backups
sudo test "$(sudo find /var/lib/sites-ci/selected-backups -name '*.sql' | wc -l)" = 2
sites_ci database backup --all --output-dir /var/lib/sites-ci/all-backups
sudo test "$(sudo find /var/lib/sites-ci/all-backups -name '*.sql' | wc -l)" = 2

for rendering in true false; do
  if [[ $rendering == true ]]; then app=fixture-ssr; else app=fixture-spa; fi
  dir=/srv/apps/$app
  sudo mkdir -p "$dir/app"
  sudo tee "$dir/package.json" >/dev/null <<'JSON'
{"name":"sites-nuxt-fixture","private":true,"type":"module","scripts":{"build":"nuxt build","postinstall":"nuxt prepare"},"dependencies":{"nuxt":"4.5.2","vue":"3.5.43","vue-router":"5.3.1"}}
JSON
  printf 'export default defineNuxtConfig({ssr: %s, devtools: {enabled: false}})\n' "$rendering" | sudo tee "$dir/nuxt.config.ts" >/dev/null
  printf '<template><h1>Sites Nuxt fixture</h1></template>\n' | sudo tee "$dir/app/app.vue" >/dev/null
  printf 'node_modules\n.output\n.nuxt\n.env\n' | sudo tee "$dir/.gitignore" >/dev/null
  sudo npm --prefix "$dir" install --package-lock-only --ignore-scripts --allow-remote=all
  fixture_git "$dir"
  sites_ci register --name "$app" --dir "$dir" --type nuxt --domain "$app.localhost"
  sites_ci deploy "$app" --no-pull
  fixture_https "$app.localhost" -o "$fixture_source/$app.html"
  if [[ $rendering == true ]]; then grep -F 'Sites Nuxt fixture' "$fixture_source/$app.html"; else grep -F '__nuxt' "$fixture_source/$app.html"; fi
  sites_ci restart "$app" web
done
sites_ci ports
sites_ci remove fixture-ssr
sites_ci remove fixture-spa
sites_ci remove fixture-octane
removed_git_uid=$(id -u sites-fixture-php)
sites_ci remove fixture-php
sudo getfacl -cn /var/lib/sites-ci/git/id_ed25519 | grep -Fx "user:$removed_git_uid:---"
sudo test -f /srv/apps/fixture-php/.env
sudo test -f /var/lib/sites-ci/credentials/fixture-php.env
sudo test "$(sudo stat -c '%U' /srv/apps/fixture-php/.env)" = root
if getent passwd sites-fixture-php; then echo 'Managed user was not removed' >&2; exit 1; fi
# Reuse retained data/credentials and verify a repeated deployment stays clean.
sites_ci register --name fixture-php --dir /srv/apps/fixture-php --type laravel --domain fixture-php.localhost --scheduler
sites_ci deploy fixture-php --no-pull
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
sites_ci remove fixture-php
sites_ci doctor
echo 'Disposable host smoke test passed.'
