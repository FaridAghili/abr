#!/usr/bin/env bash
# This changes an entire host. Run only in a disposable Ubuntu environment.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-false} != true && ${ABR_HOST_TEST:-0} != 1 ]]; then
  echo 'Refusing host test: use a disposable Ubuntu machine and ABR_HOST_TEST=1.' >&2
  exit 1
fi
abr_binary_source=$(realpath "${1:-bin/abr}")
abr_binary_directory=$(mktemp -d)
fixture_source=''
fixture_git_shim=0
trap 'rm -rf "$abr_binary_directory"; if [[ -n $fixture_source ]]; then rm -rf "$fixture_source"; fi; if [[ $fixture_git_shim == 1 ]]; then sudo rm -f /usr/local/bin/git; fi' EXIT
install -m 755 "$abr_binary_source" "$abr_binary_directory/abr"
abr_test_binary="$abr_binary_directory/abr"
abr_ci() {
  (
    # Setup must work with no repository or template files beside the binary.
    cd "$abr_binary_directory"
    sudo "$abr_test_binary" --config /etc/abr-ci/config.toml --state-dir /var/lib/abr-ci "$@"
  )
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
fixture_security_headers() {
  tr -d '\r' < "$1" | grep -Fix 'Strict-Transport-Security: max-age=15768000'
  tr -d '\r' < "$1" | grep -Fix 'X-Frame-Options: SAMEORIGIN'
  tr -d '\r' < "$1" | grep -Fix 'X-Content-Type-Options: nosniff'
  tr -d '\r' < "$1" | grep -Fix 'Referrer-Policy: strict-origin-when-cross-origin'
  if grep -Ei '^(server|x-powered-by):' "$1"; then echo 'Identifying response header leaked' >&2; exit 1; fi
}
fixture_missing_assets() {
  # Verify the reserved namespace directly, also with the upstream stopped.
  local method uri status
  for method in GET HEAD POST; do
    for uri in "$2" "$2/missing.css" "$2/missing-AbCd1234.js" "$2/nested/missing.json"; do
      local request=(--request "$method")
      if [[ $method == HEAD ]]; then request=(--head); fi
      status=$(curl --silent --show-error --max-time 10 --insecure --resolve "$1:443:127.0.0.1" \
        "${request[@]}" -D "$fixture_source/missing-headers" -o /dev/null --write-out '%{http_code}' "https://$1$uri")
      test "$status" = 404
      fixture_security_headers "$fixture_source/missing-headers"
      if grep -Ei '^cache-control:.*(immutable|max-age=31536000)' "$fixture_source/missing-headers"; then
        echo 'Missing asset received immutable caching' >&2; exit 1
      fi
    done
  done
}
fixture_redirect() {
  # Both HTTP and HTTPS must preserve path/query and return method-preserving 308.
  local scheme port redirect_status
  local schemes=(http https)
  if [[ ${3:-} == http ]]; then schemes=(http); fi
  for scheme in "${schemes[@]}"; do
    port=80
    if [[ $scheme == https ]]; then port=443; fi
    redirect_status=$(curl --fail --silent --show-error --retry 10 --retry-all-errors --retry-delay 1 \
      --max-time 10 --insecure --resolve "$1:$port:127.0.0.1" \
      --data 'redirect-fixture' -D "$fixture_source/redirect-headers" -o /dev/null \
      --write-out '%{http_code}' "$scheme://$1/preserved/path?x=1&y=2")
    test "$redirect_status" = 308
    tr -d '\r' < "$fixture_source/redirect-headers" | grep -Fix "location: https://$2/preserved/path?x=1&y=2"
    if [[ $scheme == https ]]; then
      fixture_security_headers "$fixture_source/redirect-headers"
    elif grep -Ei '^(strict-transport-security|server|x-powered-by):' "$fixture_source/redirect-headers"; then
      echo 'Unexpected HTTP redirect header' >&2; exit 1
    fi
  done
}

# GitHub runner images ship MySQL with this documented test password and other
# inactive web servers. These adjustments belong only to the disposable fixture.
if [[ ${GITHUB_ACTIONS:-false} == true ]]; then
  # runner-images deliberately makes these paths world-writable for builds.
  # Restore ordinary host directory permissions for Abr's root path checks.
  # Keep this fixture adjustment out of production provisioning.
  sudo chmod 755 /opt /usr/local/bin /usr/local/lib/node_modules
  sudo systemctl mask --now nginx apache2
  printf '[client]\nuser=root\npassword=root\n' | sudo tee /root/.my.cnf >/dev/null
  sudo chmod 600 /root/.my.cnf
fi
# A real key login prerequisite for the disposable host's root test account.
sudo apt-get install -y openssh-server
sudo install -d -m 700 /root/.ssh
sudo install -d -m 755 /run/sshd
sudo ssh-keygen -t ed25519 -N '' -f /root/.ssh/abr-fixture-ssh >/dev/null
sudo bash -c 'cat /root/.ssh/abr-fixture-ssh.pub >> /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys'
sudo apt-get install -y redis-server
sudo systemctl start redis-server
sudo redis-cli SET abr-fixture-persist survives-setup >/dev/null
sudo redis-cli SAVE >/dev/null
abr_ci setup --no-firewall --ssh-port 22 --admin-user root
for repository in caddy node; do
  repository_key="/etc/apt/keyrings/abr/$repository.gpg"
  test "$(stat -c '%u:%g:%a' /etc/apt/keyrings/abr)" = 0:0:755
  test "$(stat -c '%u:%g:%a' "$repository_key")" = 0:0:644
  sudo runuser -u _apt -- test -r "$repository_key"
  grep -Fx "Signed-By: $repository_key" "/etc/apt/sources.list.d/abr-$repository.sources"
done
test ! -d "$abr_binary_directory/templates"
sudo test -f /etc/abr/templates/caddy-site.caddy.tmpl
sudo test -f /etc/abr/templates/scheduler.service.tmpl
sudo test -f /etc/abr/templates/ssh-hardening.conf.tmpl
abr_ci config example > "$abr_binary_directory/config.example.toml"
grep -Fx 'user = "abr-api"' "$abr_binary_directory/config.example.toml"
sudo redis-cli GET abr-fixture-persist | grep -Fx survives-setup
fixture_default_headers=$(mktemp)
fixture_default_body=$(mktemp)
curl --silent --show-error http://127.0.0.1/ -D "$fixture_default_headers" -o "$fixture_default_body"
grep -E '^HTTP/[[:digit:].]+ 404' "$fixture_default_headers"
if grep -Ei '^server:' "$fixture_default_headers"; then echo 'Default HTTP header leaked' >&2; exit 1; fi
rm -f "$fixture_default_headers" "$fixture_default_body"
# Caddy administration is restricted to root and Caddy, not application users.
sudo test -S /var/lib/caddy/abr-admin.sock
if curl --silent --max-time 2 http://127.0.0.1:2019/config/ >/dev/null; then
  echo 'Caddy administration is exposed on the default TCP port' >&2; exit 1
fi
/usr/local/bin/svgo --version
/usr/local/bin/ncu --version
/usr/local/bin/composer --no-plugins --no-scripts --version
# Installers must not retain write access to the published shared toolchain.
fixture_denied _apt touch /opt/abr/node-tools/current/.abr-ci-write-probe
test "$(stat -Lc '%u:%g:%a' /opt/abr/node-tools/current)" = 0:0:755
test ! -e /opt/abr/node-tools/current/.home
sudo systemctl start ssh.service
# The host key exemption is only for this disposable localhost fixture.
sudo ssh -F /dev/null -i /root/.ssh/abr-fixture-ssh -o IdentitiesOnly=yes \
  -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=10 root@127.0.0.1 true
sudo /usr/sbin/sshd -T | grep -Fx 'passwordauthentication no'
sudo /usr/sbin/sshd -T | grep -Fx 'authenticationmethods publickey'
sudo mysql --protocol=socket --user=root --batch --skip-column-names \
  -e 'SELECT @@bind_address, @@local_infile;' | grep -Fx $'127.0.0.1\t0'
sudo redis-cli CONFIG GET bind | grep -Fx '127.0.0.1 -::1'
stat -c '%U:%G %a %n' /etc/redis
# chmod preserves the packaged directory's setgid bit for group inheritance.
case "$(stat -c '%u:%G:%a' /etc/redis)" in
  0:redis:750|0:redis:2750) ;;
  *) echo 'Unsafe Redis configuration directory permissions' >&2; exit 1 ;;
esac
sudo runuser -u redis -- head -c 1 /etc/redis/redis.conf >/dev/null
sudo runuser -u redis -- head -c 1 /etc/redis/abr.conf >/dev/null
fixture_denied redis touch /etc/redis/.abr-ci-write-probe
sudo redis-cli CONFIG GET appendonly | grep -Fx yes
sudo redis-cli CONFIG GET maxmemory-policy | grep -Fx noeviction
sudo mysql --protocol=socket --user=root --batch --skip-column-names \
  -e 'SELECT @@innodb_buffer_pool_size, @@max_connections;' | grep -Fx $'268435456\t100'
# Shared identity tests remain offline: no GitHub account or private repository.
abr_ci git setup
abr_ci git setup

# Exercise initial clone on the real host without any external repository or
# SSH account. A disposable-only shim rewrites just this fixture URL to a local
# bare repository, while preserving Abr's real runuser/setpriv invocation.
test ! -e /usr/local/bin/git
sudo /usr/bin/git init --bare /srv/abr-clone-fixture
sudo chown -hR _apt /srv/abr-clone-fixture
sudo chmod -R a+rX /srv/abr-clone-fixture
cat > "$abr_binary_directory/git-fixture" <<'SH'
#!/bin/sh
case " $* " in
  *' clone '* )
    [ "$(id -u)" != 0 ] || exit 1
    grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status || exit 1
    exec /usr/bin/git -c url.file:///srv/abr-clone-fixture.insteadOf=git@github.com:fixture/local.git "$@"
    ;;
esac
exec /usr/bin/git "$@"
SH
sudo install -m 755 "$abr_binary_directory/git-fixture" /usr/local/bin/git
fixture_git_shim=1
abr_ci clone git@github.com:fixture/local.git fixture-clone
sudo test -d /srv/apps/fixture-clone/.git
test "$(stat -c '%u' /srv/apps/fixture-clone/.git)" = 0
test -z "$(sudo find /srv/apps -maxdepth 1 -name '.abr-clone-*' -print)"
sudo rm /usr/local/bin/git
fixture_git_shim=0

# Save one account for all subsequent app installs and repeated deployments.
printf '%s\n' 'fixture-private-token' | abr_ci composer auth --host packages.example.invalid --username fixture --password-stdin
sudo test "$(sudo stat -c '%U:%a' /var/lib/abr-ci/composer/auth.json)" = root:600
fixture_denied nobody cat /var/lib/abr-ci/composer/auth.json

fixture_source=$(mktemp -d)
composer create-project --no-install --no-scripts --prefer-dist 'laravel/laravel:^13.0' "$fixture_source/laravel"
(
  cd "$fixture_source/laravel"
  composer require laravel/octane spiral/roadrunner-cli spiral/roadrunner-http --no-update --no-scripts --no-interaction
  composer update --no-install --no-scripts --no-interaction
  # npm 12's lock-only resolution incorrectly blocks bundled registry tarballs.
  # This opt-in is confined to generating our disposable fixture lockfile.
  npm install --package-lock-only --ignore-scripts --allow-remote=all
  cat > abr-fixture-privileges.php <<'PHP'
<?php
if (posix_geteuid() === 0 || !preg_match('/^NoNewPrivs:\s+1$/m', file_get_contents('/proc/self/status'))) {
    fwrite(STDERR, "Composer script has unsafe privileges\n");
    exit(1);
}
$home = getenv('COMPOSER_HOME');
$auth = $home ? json_decode(file_get_contents($home.'/auth.json'), true) : null;
if ($home !== '/var/lib/abr-ci/composer'
    || ($auth['http-basic']['packages.example.invalid']['username'] ?? '') !== 'fixture'
    || ($auth['http-basic']['packages.example.invalid']['password'] ?? '') !== 'fixture-private-token'
    || is_writable($home) || is_writable($home.'/auth.json')
    || getenv('COMPOSER_CACHE_DIR') !== getenv('HOME').'/.cache/composer') {
    fwrite(STDERR, "Shared Composer authentication or private cache is unavailable\n");
    exit(1);
}
PHP
  cat > abr-fixture-privileges.cjs <<'JS'
const fs = require('node:fs');
if (process.getuid() === 0 || !/^NoNewPrivs:\s+1$/m.test(fs.readFileSync('/proc/self/status', 'utf8'))) {
  throw new Error('npm script has unsafe privileges');
}
JS
  # Exercise arbitrary Composer and npm project scripts under the real runner.
  php -r '$p="composer.json"; $c=json_decode(file_get_contents($p),true); $c["scripts"]["pre-install-cmd"][]="@php abr-fixture-privileges.php"; file_put_contents($p,json_encode($c,JSON_PRETTY_PRINT|JSON_UNESCAPED_SLASHES)."\n");'
  node -e 'const fs=require("node:fs"); const p=JSON.parse(fs.readFileSync("package.json")); p.scripts.prebuild="node abr-fixture-privileges.cjs"; fs.writeFileSync("package.json",JSON.stringify(p,null,2)+"\n");'
)
sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-php
sudo tee /srv/apps/fixture-php/routes/web.php >/dev/null <<'PHP'
<?php
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route;
Route::get('/', fn () => response('Laravel fixture database='.DB::select('SELECT 1 AS ok')[0]->ok)->withHeaders(['X-Powered-By' => 'fixture-runtime', 'Strict-Transport-Security' => 'max-age=0', 'X-Frame-Options' => 'DENY', 'X-Content-Type-Options' => 'fixture-invalid', 'Referrer-Policy' => 'unsafe-url']));
Route::get('/php-config', fn () => response()->json(['expose_php' => ini_get('expose_php'), 'display_errors' => ini_get('display_errors'), 'opcache' => ini_get('opcache.enable'), 'unprivileged' => posix_geteuid() !== 0, 'no_new_privs' => (bool) preg_match('/^NoNewPrivs:\s+1$/m', file_get_contents('/proc/self/status'))]));
PHP

fixture_git() {
  sudo git -C "$1" init --initial-branch=main
  sudo git -C "$1" config user.name 'Abr fixture'
  sudo git -C "$1" config user.email 'fixture@example.invalid'
  sudo git -C "$1" add .
  sudo git -C "$1" commit --no-gpg-sign -m 'Fixture'
}
fixture_git /srv/apps/fixture-php
abr_ci register --name fixture-php --type laravel --domain fixture-php.localhost --serving-domain extra.fixture-php.localhost --canonical-host non-www --scheduler
# Fresh accounts must not access a foreign database whose name would match an
# unescaped underscore in a database grant.
sudo mysql --protocol=socket --user=root <<'SQL'
CREATE DATABASE fixtureXphp;
SQL
printf 'USE fixtureXphp; CREATE TABLE forbidden (id INT);\n' | sudo tee /var/lib/abr-ci/foreign.sql >/dev/null
if abr_ci database import fixture-php /var/lib/abr-ci/foreign.sql --yes; then
  echo 'Wildcard database grant exposed another database' >&2; exit 1
fi
# Provision a new account with partial revokes enabled and verify its scope.
sudo mysql --protocol=socket --user=root -e 'SET GLOBAL partial_revokes=ON;'
sudo mkdir -p /srv/apps/fixture-literal/public /srv/apps/fixture-literal/storage/app/public
abr_ci register --name fixture-literal --type laravel --domain fixture-literal.localhost
sudo mysql --protocol=socket --user=root -e 'CREATE DATABASE fixtureXliteral;'
printf 'USE fixtureXliteral; CREATE TABLE forbidden (id INT);\n' | sudo tee /var/lib/abr-ci/foreign-literal.sql >/dev/null
if abr_ci database import fixture-literal /var/lib/abr-ci/foreign-literal.sql --yes; then
  echo 'Literal grant exposed another database' >&2; exit 1
fi
abr_ci remove fixture-literal
sudo mysql --protocol=socket --user=root -e 'SET GLOBAL partial_revokes=OFF;'
sudo bash -c 'awk "!/^DB_(CONNECTION|HOST|PORT|DATABASE|USERNAME|PASSWORD)=/" /srv/apps/fixture-php/.env.example > /srv/apps/fixture-php/.env; cat /var/lib/abr-ci/credentials/fixture-php.env >> /srv/apps/fixture-php/.env; chmod 600 /srv/apps/fixture-php/.env'
abr_ci deploy fixture-php --no-pull
fixture_denied abr-fixture-php sh -c 'printf overwritten >> /var/lib/abr-ci/composer/auth.json'
fixture_denied nobody cat /var/lib/abr-ci/composer/auth.json
sudo grep -Fx 'DB_DATABASE=fixture_php' /var/lib/abr-ci/credentials/fixture-php.env
if sudo grep -R -F 'fixture-private-token' /var/lib/abr-ci/deployments; then
  echo 'Composer token leaked to deployment history' >&2; exit 1
fi
test "$(sudo systemctl show abr-fixture-php-scheduler.service --property=NoNewPrivileges --value)" = yes
test "$(sudo systemctl show abr-fixture-php-scheduler.service --property=ProtectSystem --value)" = full
fixture_denied abr-fixture-php curl --fail --silent --max-time 2 \
  --unix-socket /var/lib/caddy/abr-admin.sock http://localhost/config/
# Load the identity instead of test -r: Ubuntu's Rust test can ignore ACLs.
sudo runuser -u abr-fixture-php -- ssh-keygen -y -P '' -f /var/lib/abr-ci/git/id_ed25519 >/dev/null
abr_ci git setup
fixture_denied abr-fixture-php head -c 1 /var/lib/abr-ci/credentials/fixture-php.env
fixture_denied nobody head -c 1 /var/lib/abr-ci/git/id_ed25519
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
for private_uri in /.env /.env.production /.git/config; do
  sudo mkdir -p "/srv/apps/fixture-php/public$(dirname "$private_uri")"
  printf 'private fixture data' | sudo tee "/srv/apps/fixture-php/public$private_uri" >/dev/null
  private_status=$(curl --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
    --write-out '%{http_code}' -D "$fixture_source/private-headers" --output "$fixture_source/private-response" "https://fixture-php.localhost$private_uri")
  test "$private_status" = 404
  fixture_security_headers "$fixture_source/private-headers"
  if grep -F 'private fixture data' "$fixture_source/private-response"; then
    echo 'Private static file was exposed' >&2; exit 1
  fi
  sudo rm "/srv/apps/fixture-php/public$private_uri"
done
sudo rmdir /srv/apps/fixture-php/public/.git
fixture_redirect www.fixture-php.localhost fixture-php.localhost
for serving_domain in fixture-php.localhost extra.fixture-php.localhost; do
  fixture_redirect "$serving_domain" "$serving_domain" http
  fixture_https "$serving_domain" -D "$fixture_source/serving-headers" >/dev/null
  fixture_security_headers "$fixture_source/serving-headers"
done
sudo test -S /run/php/abr-fixture-php.sock
sudo test "$(sudo stat -c '%a' /srv/apps/fixture-php/.env)" = 600
sudo test "$(sudo stat -c '%a' /var/lib/abr-ci/credentials/fixture-php.env)" = 600
# Private atomic SQL dump and an actual restore with the scoped database account.
sudo mysql --protocol=socket --user=root fixture_php -e 'CREATE TABLE abr_backup_check (id INT PRIMARY KEY); INSERT INTO abr_backup_check VALUES (42);'
abr_ci database backup fixture-php --output-dir /var/lib/abr-ci/backups
sql_dump=$(sudo find /var/lib/abr-ci/backups -name 'fixture-php-*.sql' -print -quit)
sudo test "$(sudo stat -c '%a' "$sql_dump")" = 600
abr_ci disable fixture-php
sudo mysql --protocol=socket --user=root fixture_php -e 'DROP TABLE abr_backup_check;'
abr_ci database import fixture-php "$sql_dump" --yes
sudo mysql --protocol=socket --user=root --batch --skip-column-names fixture_php -e 'SELECT id FROM abr_backup_check;' | grep -Fx 42
# SQL import cannot escape to another database or execute a root shell command.
printf 'CREATE DATABASE forbidden_database;\n' | sudo tee /var/lib/abr-ci/forbidden.sql >/dev/null
if abr_ci database import fixture-php /var/lib/abr-ci/forbidden.sql --yes; then echo 'Unscoped SQL was allowed' >&2; exit 1; fi
printf '\\! touch /var/lib/abr-ci/import-shell-executed\n' | sudo tee /var/lib/abr-ci/shell.sql >/dev/null
if abr_ci database import fixture-php /var/lib/abr-ci/shell.sql --yes; then echo 'SQL client shell command was allowed' >&2; exit 1; fi
sudo test ! -e /var/lib/abr-ci/import-shell-executed
abr_ci enable fixture-php
fixture_https fixture-php.localhost --dump-header "$fixture_source/headers" >/dev/null
fixture_security_headers "$fixture_source/headers"
fixture_missing_assets fixture-php.localhost /build/assets
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/php-config | grep -F '"opcache":"1"'
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/php-config | grep -F '"unprivileged":true,"no_new_privs":true'
# A generated hashed asset is served with Brotli and immutable caching.
asset_path=$(sudo find /srv/apps/fixture-php/public/build/assets -name 'app-*.css' -print -quit)
sudo test -f "$asset_path.br"
asset_uri=${asset_path#/srv/apps/fixture-php/public}
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -H 'Accept-Encoding: br' --dump-header "$fixture_source/asset-headers" \
  "https://fixture-php.localhost$asset_uri" -o "$fixture_source/asset.br"
grep -Ei '^content-encoding: br' "$fixture_source/asset-headers"
grep -Fi 'Cache-Control: public, max-age=31536000, immutable' "$fixture_source/asset-headers"
fixture_security_headers "$fixture_source/asset-headers"
brotli --decompress --stdout "$fixture_source/asset.br" | sudo cmp - "$asset_path"
# A stable filename can change on the next deploy and must not be immutable.
printf 'body { color: black; }\n' | sudo tee /srv/apps/fixture-php/public/build/assets/plain.css >/dev/null
curl --fail --silent --show-error --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -D "$fixture_source/plain-headers" https://fixture-php.localhost/build/assets/plain.css -o /dev/null
fixture_security_headers "$fixture_source/plain-headers"
if grep -Ei '^cache-control:.*(immutable|max-age=31536000)' "$fixture_source/plain-headers"; then
  echo 'Unversioned asset received immutable caching' >&2; exit 1
fi
for encoding in zstd gzip; do
  curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
    -H "Accept-Encoding: $encoding" --dump-header "$fixture_source/$encoding-headers" \
    "https://fixture-php.localhost$asset_uri" -o /dev/null
  grep -Ei "^content-encoding: $encoding" "$fixture_source/$encoding-headers"
done
curl --silent --resolve fixture-php.localhost:80:127.0.0.1 -D "$fixture_source/http-headers" http://fixture-php.localhost/ -o /dev/null
if grep -Ei '^server:' "$fixture_source/http-headers"; then echo 'HTTP redirect header leaked' >&2; exit 1; fi
abr_ci restart fixture-php web
abr_ci disable fixture-php
abr_ci enable fixture-php
abr_ci status fixture-php
abr_ci logs fixture-php scheduler

sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-octane
sudo cp /srv/apps/fixture-php/routes/web.php /srv/apps/fixture-octane/routes/web.php
printf '\n.rr.yaml\n' | sudo tee -a /srv/apps/fixture-octane/.gitignore >/dev/null
fixture_git /srv/apps/fixture-octane
abr_ci register --name fixture-octane --type laravel --web-driver octane --domain fixture-octane.localhost --canonical-host www
sudo bash -c 'awk "!/^DB_(CONNECTION|HOST|PORT|DATABASE|USERNAME|PASSWORD)=/" /srv/apps/fixture-octane/.env.example > /srv/apps/fixture-octane/.env; cat /var/lib/abr-ci/credentials/fixture-octane.env >> /srv/apps/fixture-octane/.env; chmod 600 /srv/apps/fixture-octane/.env'
abr_ci deploy fixture-octane --no-pull
fixture_https www.fixture-octane.localhost -D "$fixture_source/octane-headers" | grep -F 'Laravel fixture database=1'
fixture_security_headers "$fixture_source/octane-headers"
fixture_redirect www.fixture-octane.localhost www.fixture-octane.localhost http
fixture_redirect fixture-octane.localhost www.fixture-octane.localhost
sudo test ! -f /srv/apps/fixture-octane/rr
sudo test -x /usr/local/bin/rr
abr_ci restart fixture-octane web
# Exercise Caddy's error route while the disposable upstream is unavailable.
sudo systemctl stop abr-fixture-octane-octane.service
error_status=$(curl --silent --show-error --insecure --resolve www.fixture-octane.localhost:443:127.0.0.1 \
  -D "$fixture_source/error-headers" -o /dev/null --write-out '%{http_code}' https://www.fixture-octane.localhost/)
test "$error_status" = 502
fixture_security_headers "$fixture_source/error-headers"
fixture_missing_assets www.fixture-octane.localhost /build/assets
sudo systemctl start abr-fixture-octane-octane.service
abr_ci database backup fixture-php fixture-octane --output-dir /var/lib/abr-ci/selected-backups
sudo test "$(sudo find /var/lib/abr-ci/selected-backups -name '*.sql' | wc -l)" = 2
abr_ci database backup --all --output-dir /var/lib/abr-ci/all-backups
sudo test "$(sudo find /var/lib/abr-ci/all-backups -name '*.sql' | wc -l)" = 2

for rendering in true false; do
  if [[ $rendering == true ]]; then app=fixture-ssr; else app=fixture-spa; fi
  dir=/srv/apps/$app
  # These apps are subdomains of the canonical-host fixtures, with no www policy.
  if [[ $rendering == true ]]; then
    app_domain=api.fixture-octane.localhost
  else
    app_domain=portal.fixture-php.localhost
  fi
  nuxt_source="$fixture_source/$app"
  mkdir -p "$nuxt_source/app"
  tee "$nuxt_source/package.json" >/dev/null <<'JSON'
{"name":"abr-nuxt-fixture","private":true,"type":"module","scripts":{"prebuild":"node abr-fixture-privileges.cjs","build":"nuxt build","postinstall":"nuxt prepare"},"dependencies":{"nuxt":"4.5.2","vue":"3.5.43","vue-router":"5.3.1"}}
JSON
  printf 'export default defineNuxtConfig({ssr: %s, devtools: {enabled: false}})\n' "$rendering" | tee "$nuxt_source/nuxt.config.ts" >/dev/null
  printf '<template><h1>Abr Nuxt fixture</h1></template>\n' | tee "$nuxt_source/app/app.vue" >/dev/null
  printf 'node_modules\n.output\n.nuxt\n.env\n' | tee "$nuxt_source/.gitignore" >/dev/null
  cp "$fixture_source/laravel/abr-fixture-privileges.cjs" "$nuxt_source/"
  npm --prefix "$nuxt_source" install --package-lock-only --ignore-scripts --allow-remote=all
  sudo cp -R "$nuxt_source" "$dir"
  fixture_git "$dir"
  abr_ci register --name "$app" --type nuxt --domain "$app_domain"
  abr_ci deploy "$app" --no-pull
  test "$(sudo systemctl show "abr-$app-nuxt.service" --property=NoNewPrivileges --value)" = yes
  nuxt_pid=$(sudo systemctl show "abr-$app-nuxt.service" --property=MainPID --value)
  sudo awk '/^NoNewPrivs:/ {if ($2 != 1) exit 1; found=1} END {if (!found) exit 1}' "/proc/$nuxt_pid/status"
  fixture_redirect "$app_domain" "$app_domain" http
  fixture_https "$app_domain" -D "$fixture_source/nuxt-page-headers" -o "$fixture_source/$app.html"
  fixture_security_headers "$fixture_source/nuxt-page-headers"
  if [[ $rendering == true ]]; then grep -F 'Abr Nuxt fixture' "$fixture_source/$app.html"; else grep -F '__nuxt' "$fixture_source/$app.html"; fi
  nuxt_asset=$(sudo find "$dir/.output/public/_nuxt" -type f -name '*.js' -size +511c -print -quit)
  sudo test -f "$nuxt_asset.br"
  nuxt_uri=${nuxt_asset#"$dir/.output/public"}
  curl --fail --silent --insecure --resolve "$app_domain:443:127.0.0.1" \
    -H 'Accept-Encoding: br' -D "$fixture_source/nuxt-headers" \
    "https://$app_domain$nuxt_uri" -o "$fixture_source/nuxt.br"
  grep -Ei '^content-encoding: br' "$fixture_source/nuxt-headers"
  grep -Fi 'Cache-Control: public, max-age=31536000, immutable' "$fixture_source/nuxt-headers"
  fixture_security_headers "$fixture_source/nuxt-headers"
  brotli --decompress --stdout "$fixture_source/nuxt.br" | sudo cmp - "$nuxt_asset"
  sudo systemctl stop "abr-$app-nuxt.service"
  fixture_missing_assets "$app_domain" /_nuxt
  sudo systemctl start "abr-$app-nuxt.service"
  abr_ci restart "$app" web
done
abr_ci ports
abr_ci remove fixture-ssr --purge --yes
sudo test ! -e /srv/apps/fixture-ssr
sudo test ! -e /var/lib/abr-users/abr-fixture-ssr
sudo test ! -e /etc/systemd/system/abr-fixture-ssr-nuxt.service
if getent passwd abr-fixture-ssr || getent group abr-fixture-ssr; then
  echo 'Fully removed Nuxt app retained its Ubuntu account/group' >&2; exit 1
fi
abr_ci remove fixture-spa
abr_ci remove fixture-octane
removed_git_uid=$(id -u abr-fixture-php)
abr_ci remove fixture-php
sudo getfacl -cn /var/lib/abr-ci/git/id_ed25519 | grep -Fx "user:$removed_git_uid:---"
sudo test -f /srv/apps/fixture-php/.env
sudo test -f /var/lib/abr-ci/credentials/fixture-php.env
sudo test "$(sudo stat -c '%U' /srv/apps/fixture-php/.env)" = root
if getent passwd abr-fixture-php; then echo 'Managed user was not removed' >&2; exit 1; fi
# Reuse retained data/credentials and verify a repeated deployment stays clean.
abr_ci register --name fixture-php --type laravel --domain fixture-php.localhost --scheduler
abr_ci deploy fixture-php --no-pull
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
if abr_ci remove fixture-php --purge; then
  echo 'Full removal did not require explicit confirmation' >&2; exit 1
fi
abr_ci remove fixture-php --purge --dry-run
sudo test -f /srv/apps/fixture-php/.env
# Bind mounts must not cause deletion of storage outside the registered project.
mkdir -p "$fixture_source/mounted-uploads"
touch "$fixture_source/mounted-uploads/keep"
sudo mkdir -p /srv/apps/fixture-php/storage/abr-mount-fixture
sudo mount --bind "$fixture_source/mounted-uploads" /srv/apps/fixture-php/storage/abr-mount-fixture
if abr_ci remove fixture-php --purge --yes; then
  echo 'Full removal traversed a mounted directory' >&2; exit 1
fi
sudo test -f "$fixture_source/mounted-uploads/keep"
sudo umount /srv/apps/fixture-php/storage/abr-mount-fixture
purged_git_uid=$(id -u abr-fixture-php)
abr_ci remove fixture-php --purge --yes
for path in /srv/apps/fixture-php /var/lib/abr-users/abr-fixture-php \
  /var/lib/abr-ci/apps/fixture-php.json /var/lib/abr-ci/users/abr-fixture-php.json \
  /var/lib/abr-ci/databases/fixture-php.json /var/lib/abr-ci/credentials/fixture-php.env \
  /var/lib/abr-ci/env/fixture-php.env /var/lib/abr-ci/deployments/fixture-php \
  /etc/caddy/abr.d/abr-fixture-php.caddy /etc/php/8.5/fpm/pool.d/abr-fixture-php.conf \
  /etc/systemd/system/abr-fixture-php-scheduler.service /etc/systemd/system/abr-fixture-php-scheduler.timer; do
  sudo test ! -e "$path"
done
if getent passwd abr-fixture-php || getent group abr-fixture-php; then
  echo 'Fully removed app retained its Ubuntu account/group' >&2; exit 1
fi
if abr_ci list | grep -F fixture-php; then
  echo 'Fully removed app retained its configuration entry' >&2; exit 1
fi
if abr_ci ports | grep -F fixture-php; then
  echo 'Fully removed app retained port reservations' >&2; exit 1
fi
sudo mysql --protocol=socket --user=root --batch --skip-column-names <<'SQL' | grep -Fx 0
SELECT (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='fixture_php') + (SELECT COUNT(*) FROM mysql.user WHERE User='abr-fixture-php' AND Host='localhost');
SQL
# Ordinary removals and foreign databases must still retain their data.
sudo test -f /srv/apps/fixture-octane/.env
sudo test -f /var/lib/abr-ci/git/id_ed25519
for path in /var/lib/abr-ci /var/lib/abr-ci/git/id_ed25519 /var/lib/abr-ci/composer/auth.json; do
  if sudo getfacl -cn "$path" | grep -E "^user:$purged_git_uid:"; then
    echo 'Fully removed app retained a shared credential ACL' >&2; exit 1
  fi
done
sudo mysql --protocol=socket --user=root --batch --skip-column-names <<'SQL' | grep -Fx 2
SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME IN ('fixture_octane', 'fixtureXphp');
SQL
sudo test -f /var/lib/abr-ci/composer/auth.json
if sudo getfacl -cp /var/lib/abr-ci/composer/auth.json | grep -E '^user:[^:]+:r'; then
  echo 'Removed app retained Composer token access' >&2; exit 1
fi
# The same app/database/account names must be available for a fresh registration.
sudo mkdir -p /srv/apps/fixture-php/public /srv/apps/fixture-php/storage/app/public
abr_ci register --name fixture-php --type laravel --domain fixture-php.localhost
abr_ci remove fixture-php --purge --yes
abr_ci doctor
echo 'Disposable host smoke test passed.'
