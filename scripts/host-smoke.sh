#!/usr/bin/env bash
# This changes an entire host. Run only in a disposable Ubuntu environment.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-false} != true && ${ABR_HOST_TEST:-0} != 1 ]]; then
  echo 'Refusing host test: use a disposable Ubuntu machine and ABR_HOST_TEST=1.' >&2
  exit 1
fi
abr_smoke_script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
abr_binary_source=$(realpath "${1:-bin/abr}")
abr_binary_directory=$(mktemp -d)
fixture_source=''
fixture_git_shim=0
trap 'sudo systemctl stop abr-ci-foreign-listener.service >/dev/null 2>&1 || true; if [[ -S "$abr_binary_directory/admin-tunnel" ]]; then sudo ssh -S "$abr_binary_directory/admin-tunnel" -O exit root@127.0.0.1; fi; sudo rm -rf "$abr_binary_directory"; if [[ -n $fixture_source ]]; then sudo rm -rf "$fixture_source"; fi; if [[ $fixture_git_shim == 1 ]]; then sudo rm -f /usr/local/bin/git; fi' EXIT
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
fixture_refused() {
  local expected=$1
  shift
  local output="$abr_binary_directory/expected-refusal.log"
  if abr_ci "$@" > "$output" 2>&1; then
    cat "$output" >&2
    echo "Expected refusal: $expected" >&2; exit 1
  fi
  if ! grep -Fq "$expected" "$output"; then
    cat "$output" >&2
    echo "Unexpected failure instead of: $expected" >&2; exit 1
  fi
  echo "Verified refusal: $expected"
}
fixture_caddy_format() {
  # fmt exits nonzero if formatting differs; never rewrite configs in this check.
  sudo caddy fmt "$1" > /dev/null
}
fixture_https() {
  # Local Caddy certificates can finish issuance shortly after configuration reload.
  curl --fail --silent --show-error --retry 10 --retry-all-errors --retry-delay 1 \
    --max-time 10 --insecure --resolve "$1:443:127.0.0.1" "https://$1/" "${@:2}"
}
fixture_security_headers() {
  tr -d '\r' < "$1" | grep -Fix 'Strict-Transport-Security: max-age=15768000' > /dev/null
  tr -d '\r' < "$1" | grep -Fix 'X-Frame-Options: SAMEORIGIN' > /dev/null
  tr -d '\r' < "$1" | grep -Fix 'X-Content-Type-Options: nosniff' > /dev/null
  tr -d '\r' < "$1" | grep -Fix 'Referrer-Policy: strict-origin-when-cross-origin' > /dev/null
  if grep -Ei '^(server|via|x-powered-by):' "$1"; then echo 'Identifying response header leaked' >&2; exit 1; fi
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
fixture_image_cache() {
  # Verify real Caddy-user access to images created by the dedicated app user.
  local domain=$1 root=$2 app_user=$3 ext method
  for ext in png jpg jpeg gif avif webp svg PNG; do
    sudo runuser -u "$app_user" -- sh -c 'printf "public image fixture" > "$1"' sh "$root/abr-image.$ext"
    for method in GET HEAD; do
      local request=(--request "$method")
      if [[ $method == HEAD ]]; then request=(--head); fi
      curl --fail --silent --show-error --insecure --resolve "$domain:443:127.0.0.1" \
        "${request[@]}" -D "$fixture_source/image-headers" -o "$fixture_source/image-body" \
        "https://$domain/abr-image.$ext?v=1"
      tr -d '\r' < "$fixture_source/image-headers" | grep -Fix 'Cache-Control: public, max-age=2592000'
      fixture_security_headers "$fixture_source/image-headers"
      if [[ $method == GET ]]; then
        test "$(cat "$fixture_source/image-body")" = 'public image fixture'
      fi
    done
    sudo runuser -u "$app_user" -- rm -- "$root/abr-image.$ext"
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
    elif grep -Ei '^(strict-transport-security|server|via|x-powered-by):' "$fixture_source/redirect-headers"; then
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
# Keep scheduled distribution upgrades from racing our repeated apt installs.
# This override belongs only to the disposable fixture; setup still writes its
# production update policy and enables the real systemd timers.
printf 'APT::Periodic::Enable "0";\n' | sudo tee /etc/apt/apt.conf.d/zz-abr-ci-periodic >/dev/null
# A real key login prerequisite for the disposable host's root test account.
sudo apt-get install -y openssh-server
sudo install -d -m 700 /root/.ssh
sudo install -d -m 755 /run/sshd
sudo ssh-keygen -t ed25519 -N '' -f /root/.ssh/abr-fixture-ssh >/dev/null
sudo bash -c 'cat /root/.ssh/abr-fixture-ssh.pub >> /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys'
# Start with Ubuntu's older native Caddy package to exercise setup upgrades.
sudo apt-get install -y caddy
caddy_ubuntu_version=$(dpkg-query -W -f='${Version}' caddy)
sudo apt-get install -y redis-server
sudo systemctl start redis-server
sudo redis-cli SET abr-fixture-persist survives-setup >/dev/null
sudo redis-cli SAVE >/dev/null
# Exercise preservation of an existing shell configuration on this disposable host.
printf '%s\n' 'plugins=(git)' "alias abr_shell_fixture='printf preserved'" | sudo tee /root/.zshrc >/dev/null
sudo chmod 600 /root/.zshrc
sudo cp /root/.zshrc "$abr_binary_directory/zshrc-before"
fixture_shell() {
  test "$(getent passwd root | cut -d: -f7)" = /usr/bin/zsh
  sudo env -i HOME=/root USER=root LOGNAME=root PATH=/usr/local/bin:/usr/bin:/bin TERM=xterm \
    zsh -i -c '[[ $plugins[-1] == zsh-syntax-highlighting && ${plugins[(Ie)git]} -gt 0 && ${plugins[(Ie)zsh-autosuggestions]} -gt 0 && ${+functions[_zsh_autosuggest_start]} == 1 && ${+functions[_zsh_highlight]} == 1 ]] && [[ $(abr_shell_fixture) == preserved ]]'
  test "$(sudo grep -Fc '# Begin abr shell' /root/.zshrc)" = 1
}
abr_ci setup --hostname abr-ci-vps --no-firewall --ssh-port 22 --admin-user root
fixture_shell
sudo cmp /root/.zshrc.pre-abr "$abr_binary_directory/zshrc-before"
test "$(hostname)" = abr-ci-vps
test "$(hostnamectl --static hostname)" = abr-ci-vps
grep -Fx $'127.0.1.1\tabr-ci-vps' /etc/hosts
getent hosts abr-ci-vps >/dev/null
grep -Fx 'preserve_hostname: true' /etc/cloud/cloud.cfg.d/99-abr-hostname.cfg
grep -Fx 'manage_etc_hosts: false' /etc/cloud/cloud.cfg.d/99-abr-hostname.cfg
fixture_caddy_version() {
  local stable installed
  stable=$(curl --fail --silent --show-error https://api.github.com/repos/caddyserver/caddy/releases/latest | python3 -c 'import json,sys; r=json.load(sys.stdin); assert not r["draft"] and not r["prerelease"]; print(r["tag_name"][1:])')
  installed=$(dpkg-query -W -f='${Version}' caddy)
  dpkg --compare-versions "$installed" ge "$stable"
  test "$(caddy version | cut -d' ' -f1)" = "v$installed"
  sudo systemctl is-active --quiet caddy
}
fixture_caddy_version
/usr/local/bin/rr --version | grep -E 'version 2025\.1\.[0-9]+'
fixture_caddy_format /etc/caddy/Caddyfile
for repository in node; do
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
if grep -Ei '^(server|via|x-powered-by):' "$fixture_default_headers"; then echo 'Default HTTP header leaked' >&2; exit 1; fi
rm -f "$fixture_default_headers" "$fixture_default_body"
# Caddy administration is restricted to root and Caddy, not application users.
sudo test -S /var/lib/caddy/abr-admin.sock
if curl --silent --max-time 2 http://127.0.0.1:2019/config/ >/dev/null; then
  echo 'Caddy administration is exposed on the default TCP port' >&2; exit 1
fi
/usr/local/bin/svgo --version
/usr/local/bin/ncu --version
/usr/local/bin/composer --no-plugins --no-scripts --version
# Ordinary global npm/ncu commands must see the manager's shared installation.
node_global_env=(env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/nonexistent NPM_CONFIG_USERCONFIG=/nonexistent/user.npmrc NPM_CONFIG_GLOBALCONFIG=/nonexistent/global.npmrc)
test "$("${node_global_env[@]}" /usr/local/bin/npm prefix --global)" = /opt/abr/node-tools/current
# Force a real registry-backed SVGO upgrade; leave unrelated native globals alone.
# Only disposable CI metadata is changed; no package installer runs as root.
sudo cp /var/lib/abr-ci/mysql-admin.json "$abr_binary_directory/admin-before-update.json"
node_native_root=$(sudo env -i PATH=/usr/bin:/bin HOME=/nonexistent NPM_CONFIG_USERCONFIG=/nonexistent/user.npmrc NPM_CONFIG_GLOBALCONFIG=/nonexistent/global.npmrc /usr/bin/npm root --global)
sudo test ! -e "$node_native_root/is-number"
sudo mkdir "$node_native_root/is-number"
printf '{"name":"is-number","version":"6.0.0"}\n' | sudo tee "$node_native_root/is-number/package.json" >/dev/null
sudo python3 - <<'PYUPDATE'
import json
from pathlib import Path
path = Path('/opt/abr/node-tools/current/lib/node_modules/svgo/package.json')
package = json.loads(path.read_text())
package['version'] = '0.0.0'
path.write_text(json.dumps(package))
PYUPDATE
# Rewind each newly cloned repository so update must make real progress.
shell_repositories=(/root/.oh-my-zsh /root/.oh-my-zsh/custom/plugins/zsh-autosuggestions /root/.oh-my-zsh/custom/plugins/zsh-syntax-highlighting)
for shell_repository in "${shell_repositories[@]}"; do
  sudo git -C "$shell_repository" fetch --deepen=1 origin master
  sudo git -C "$shell_repository" rev-parse HEAD | sudo tee "$abr_binary_directory/$(basename "$shell_repository").head" >/dev/null
  sudo git -C "$shell_repository" reset --hard HEAD~1
done
sudo cp /root/.zshrc "$abr_binary_directory/zshrc-before-update"
# Downgrade only this disposable fixture to exercise abr update's GitHub upgrade.
sudo apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold \
  install -y --allow-downgrades "caddy=$caddy_ubuntu_version"
sudo cp /etc/caddy/Caddyfile "$abr_binary_directory/caddy-before-update"
sudo cp /etc/systemd/system/caddy.service.d/abr-admin.conf "$abr_binary_directory/caddy-admin-before-update"
abr_ci update
fixture_caddy_version
sudo cmp /etc/caddy/Caddyfile "$abr_binary_directory/caddy-before-update"
sudo cmp /etc/systemd/system/caddy.service.d/abr-admin.conf "$abr_binary_directory/caddy-admin-before-update"
sudo test -S /var/lib/caddy/abr-admin.sock
fixture_shell
sudo cmp /root/.zshrc "$abr_binary_directory/zshrc-before-update"
for shell_repository in "${shell_repositories[@]}"; do
  shell_expected=$(sudo cat "$abr_binary_directory/$(basename "$shell_repository").head")
  sudo git -C "$shell_repository" merge-base --is-ancestor "$shell_expected" HEAD
done
sudo cmp /var/lib/abr-ci/mysql-admin.json "$abr_binary_directory/admin-before-update.json"
sudo redis-cli GET abr-fixture-persist | grep -Fx survives-setup
/usr/local/bin/composer --no-plugins --no-scripts --version
/usr/local/bin/svgo --version
"${node_global_env[@]}" /usr/local/bin/npm list --global --depth=0 --json > "$abr_binary_directory/updated-globals.json"
python3 - "$abr_binary_directory/updated-globals.json" <<'PYUPDATE'
import json, sys
packages = json.load(open(sys.argv[1]))['dependencies']
assert packages['svgo']['version'] != '0.0.0'
assert 'is-number' not in packages
assert 'npm' in packages and 'npm-check-updates' in packages
PYUPDATE
sudo python3 - "$node_native_root/is-number/package.json" <<'PYUPDATE'
import json, sys
assert json.load(open(sys.argv[1]))['version'] == '6.0.0'
PYUPDATE
# Read-only installed tools remain inaccessible to their installer after updating.
# Installers must not retain write access to the published shared toolchain.
fixture_denied _apt touch /opt/abr/node-tools/current/.abr-ci-write-probe
test "$(stat -Lc '%u:%g:%a' /opt/abr/node-tools/current)" = 0:0:755
test ! -e /opt/abr/node-tools/current/.home
sudo systemctl start ssh.service
# Trust only this disposable fixture's host key, with normal strict checking.
sudo sh -c 'printf "127.0.0.1 "; cat /etc/ssh/ssh_host_ed25519_key.pub' > "$abr_binary_directory/known_hosts"
sudo ssh -F /dev/null -i /root/.ssh/abr-fixture-ssh -o IdentitiesOnly=yes \
  -o BatchMode=yes -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$abr_binary_directory/known_hosts" \
  -o ConnectTimeout=10 root@127.0.0.1 true
# Verify the TablePlus connection through an actual SSH forward, keeping its
# password in a root-only options file, never in logs or command arguments.
abr_ci database --admin
sudo test "$(sudo stat -c '%U:%a' /var/lib/abr-ci/mysql-admin.json)" = root:600
fixture_denied nobody cat /var/lib/abr-ci/mysql-admin.json
sudo cp /var/lib/abr-ci/mysql-admin.json "$abr_binary_directory/admin-before.json"
sudo python3 - /var/lib/abr-ci/mysql-admin.json "$abr_binary_directory/admin.cnf" <<'PY'
import json, os, sys
with open(sys.argv[1]) as record:
    credentials = json.load(record)
with os.fdopen(os.open(sys.argv[2], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as options:
    options.write('[client]\nuser=root\npassword=' + credentials['password'] + '\nprotocol=TCP\nhost=127.0.0.1\nport=13306\n')
PY
sudo ssh -F /dev/null -i /root/.ssh/abr-fixture-ssh -o IdentitiesOnly=yes \
  -o BatchMode=yes -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$abr_binary_directory/known_hosts" \
  -o ExitOnForwardFailure=yes -o ConnectTimeout=10 -M -S "$abr_binary_directory/admin-tunnel" \
  -fN -L 127.0.0.1:13306:127.0.0.1:3306 root@127.0.0.1
sudo mysql --defaults-file="$abr_binary_directory/admin.cnf" --no-login-paths --batch --skip-column-names \
  -e 'SELECT CURRENT_USER();' | grep -Fx root@127.0.0.1
sudo mysql --defaults-file="$abr_binary_directory/admin.cnf" --no-login-paths <<'SQL'
CREATE DATABASE abr_admin_fixture;
CREATE TABLE abr_admin_fixture.test (id INT);
INSERT INTO abr_admin_fixture.test VALUES (1);
SELECT * FROM mysql.user LIMIT 0;
DROP DATABASE abr_admin_fixture;
SQL
sudo ssh -S "$abr_binary_directory/admin-tunnel" -O exit root@127.0.0.1
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
sudo tail -c 11 /var/lib/abr-ci/git/id_ed25519.pub | grep -Fx abr-ci-vps

# Exercise initial clone on the real host without any external repository or
# SSH account. A disposable-only shim rewrites just this fixture URL to a local
# bare repository, while preserving Abr's real runuser/setpriv invocation.
test ! -e /usr/local/bin/git
sudo /usr/bin/git init --bare --initial-branch=main /srv/abr-clone-fixture
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
  # The skeleton pins concurrently's vulnerable shell-quote dependency.
  npm pkg set overrides.shell-quote=1.12.0
  # npm 12's lock-only resolution incorrectly blocks bundled registry tarballs.
  # This opt-in is confined to generating our disposable fixture lockfile.
  npm install --package-lock-only --ignore-scripts --allow-remote=all --no-fund
  npm audit --audit-level=critical
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
  # Exercise Composer/npm privileges and a frontend build that invokes Artisan.
  php -r '$p="composer.json"; $c=json_decode(file_get_contents($p),true); $c["scripts"]["pre-install-cmd"][]="@php abr-fixture-privileges.php"; file_put_contents($p,json_encode($c,JSON_PRETTY_PRINT|JSON_UNESCAPED_SLASHES)."\n");'
  node -e 'const fs=require("node:fs"); const p=JSON.parse(fs.readFileSync("package.json")); p.scripts.prebuild="node abr-fixture-privileges.cjs && php artisan list --raw --no-interaction > /dev/null"; fs.writeFileSync("package.json",JSON.stringify(p,null,2)+"\n");'
)
sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-php
sudo tee /srv/apps/fixture-php/routes/web.php >/dev/null <<'PHP'
<?php
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route;
Route::get('/', fn () => response('Laravel fixture database='.DB::select('SELECT 1 AS ok')[0]->ok)->withHeaders(['X-Powered-By' => 'fixture-runtime', 'Strict-Transport-Security' => 'max-age=0', 'X-Frame-Options' => 'DENY', 'X-Content-Type-Options' => 'fixture-invalid', 'Referrer-Policy' => 'unsafe-url']));
Route::get('/ads/banner', fn () => response('dynamic banner fixture')->withHeaders(['X-Frame-Options' => 'DENY', 'Content-Security-Policy' => "script-src 'self'"]));
Route::get('/php-config', fn () => response()->json(['expose_php' => ini_get('expose_php'), 'display_errors' => ini_get('display_errors'), 'opcache' => ini_get('opcache.enable'), 'unprivileged' => posix_geteuid() !== 0, 'no_new_privs' => (bool) preg_match('/^NoNewPrivs:\s+1$/m', file_get_contents('/proc/self/status'))]));
PHP

fixture_git() {
  sudo git -C "$1" init --initial-branch=main
  sudo git -C "$1" config user.name 'Abr fixture'
  sudo git -C "$1" config user.email 'fixture@example.invalid'
  sudo git -C "$1" add .
  sudo git -C "$1" commit --no-gpg-sign -m 'Fixture'
}
sudo tee /srv/apps/fixture-php/public/banner.html >/dev/null <<'HTML'
<!doctype html><html><body>static banner fixture</body></html>
HTML
sudo tee -a /srv/apps/fixture-php/routes/console.php >/dev/null <<'PHP'

\Illuminate\Support\Facades\Artisan::command('abr:access-check {value}', function () {
    if (posix_geteuid() !== posix_getpwnam('abr-fixture-php')['uid'] ||
        posix_getegid() !== posix_getpwnam('abr-fixture-php')['gid'] ||
        getcwd() !== '/srv/apps/fixture-php' || getenv('HOME') !== '/var/lib/abr-users/abr-fixture-php' ||
        getenv('APP_ENV') !== 'production' || getenv('APP_DEBUG') !== 'false' ||
        !preg_match('/^NoNewPrivs:\s+1$/m', file_get_contents('/proc/self/status')) ||
        $this->argument('value') !== 'literal $(id) value' || $this->ask('Fixture input') !== 'stdin works') {
        $this->error('Artisan access check failed');
        return 1;
    }
    file_put_contents(storage_path('abr-access-check'), 'app-owned');
    $this->info('Artisan access verified');
});
PHP
fixture_git /srv/apps/fixture-php
abr_ci register --name fixture-php --type laravel --domain fixture-php.localhost --serving-domain extra.fixture-php.localhost --canonical-host non-www --scheduler --build-order composer-first --embed-path /banner.html --embed-path '/ads/*' --embed-origin '*'
# Database identities use the database name, separately from Ubuntu app users.
sudo grep -Fx 'DB_DATABASE=fixture_php' /var/lib/abr-ci/credentials/fixture-php.env
sudo grep -Fx 'DB_USERNAME=fixture_php' /var/lib/abr-ci/credentials/fixture-php.env
sudo mysql --protocol=socket --user=root --batch --skip-column-names <<'SQL' | grep -Fx 2
SELECT COUNT(*) FROM mysql.user WHERE User='fixture_php' AND Host IN ('localhost','127.0.0.1');
SQL
# Fresh accounts must not access a foreign database whose name would match an
# unescaped underscore in a database grant.
sudo mysql --protocol=socket --user=root <<'SQL'
CREATE DATABASE fixtureXphp;
SQL
printf 'USE fixtureXphp; CREATE TABLE forbidden (id INT);\n' | sudo tee /var/lib/abr-ci/foreign.sql >/dev/null
fixture_refused 'import failed; database may be partially changed' database import fixture-php /var/lib/abr-ci/foreign.sql --yes
# Provision a new account with partial revokes enabled and verify its scope.
sudo mysql --protocol=socket --user=root -e 'SET GLOBAL partial_revokes=ON;'
sudo mkdir -p /srv/apps/fixture-literal/public /srv/apps/fixture-literal/storage/app/public
abr_ci register --name fixture-literal --type laravel --domain fixture-literal.localhost
sudo mysql --protocol=socket --user=root -e 'CREATE DATABASE fixtureXliteral;'
printf 'USE fixtureXliteral; CREATE TABLE forbidden (id INT);\n' | sudo tee /var/lib/abr-ci/foreign-literal.sql >/dev/null
fixture_refused 'import failed; database may be partially changed' database import fixture-literal /var/lib/abr-ci/foreign-literal.sql --yes
abr_ci remove fixture-literal
sudo mysql --protocol=socket --user=root -e 'SET GLOBAL partial_revokes=OFF;'
sudo rm -f /srv/apps/fixture-php/.env
abr_ci env fixture-php
sudo test "$(sudo stat -c '%U:%a' /srv/apps/fixture-php/.env)" = abr-fixture-php:600
sudo grep -Fx 'APP_ENV=production' /srv/apps/fixture-php/.env
sudo grep -Fx 'APP_DEBUG=false' /srv/apps/fixture-php/.env
sudo test ! -f /srv/apps/fixture-php/vendor/autoload.php
abr_ci deploy fixture-php --no-pull
printf 'stdin works\n' | abr_ci artisan fixture-php abr:access-check 'literal $(id) value'
test "$(sudo stat -c '%U:%G' /srv/apps/fixture-php/storage/abr-access-check)" = abr-fixture-php:abr-fixture-php
# The ownership probe is outside Laravel's ignored runtime directories. Remove
# only this disposable test file so subsequent deploy preflight tests stay clean.
sudo runuser -u abr-fixture-php -- rm -- /srv/apps/fixture-php/storage/abr-access-check
abr_ci artisan fixture-php cache:clear --no-interaction
fixture_refused 'not defined' artisan fixture-php abr:missing-command
test -z "$(sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php status --porcelain --untracked-files=all)"
sudo cp /srv/apps/fixture-php/.env "$abr_binary_directory/prepared-php.env"
abr_ci env fixture-php
sudo cmp /srv/apps/fixture-php/.env "$abr_binary_directory/prepared-php.env"
test -x /usr/bin/nano
fixture_caddy_format /etc/caddy/abr.d/abr-fixture-php.caddy
fixture_denied abr-fixture-php sh -c 'printf overwritten >> /var/lib/abr-ci/composer/auth.json'
fixture_denied nobody cat /var/lib/abr-ci/composer/auth.json
sudo grep -Fx 'DB_DATABASE=fixture_php' /var/lib/abr-ci/credentials/fixture-php.env
sudo grep -Fx 'DB_HOST=127.0.0.1' /var/lib/abr-ci/credentials/fixture-php.env
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
fixture_denied abr-fixture-php cat /var/lib/abr-ci/mysql-admin.json
fixture_denied nobody head -c 1 /var/lib/abr-ci/git/id_ed25519
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
# Real FPM/static responses must apply the per-app iframe exception after upstream
# headers, without changing protection on other paths or losing unrelated CSP.
for domain in fixture-php.localhost extra.fixture-php.localhost; do
  for path in /banner.html /ads/banner; do
    curl --fail --silent --show-error --insecure --resolve "$domain:443:127.0.0.1" \
      -D "$fixture_source/banner-headers" -o "$fixture_source/banner-body" "https://$domain$path?campaign=1"
    tr -d '\r' < "$fixture_source/banner-headers" | grep -Fix 'Content-Security-Policy: frame-ancestors *;'
    if grep -Ei '^x-frame-options:' "$fixture_source/banner-headers"; then
      echo 'Iframe exception still blocks embedding' >&2; exit 1
    fi
    if [[ $path == /banner.html ]]; then
      grep -F 'static banner fixture' "$fixture_source/banner-body"
    else
      grep -F 'dynamic banner fixture' "$fixture_source/banner-body"
      tr -d '\r' < "$fixture_source/banner-headers" | grep -Fix "Content-Security-Policy: script-src 'self'"
    fi
  done
  fixture_https "$domain" -D "$fixture_source/protected-headers" -o /dev/null
  fixture_security_headers "$fixture_source/protected-headers"
done

# Failed preflight must leave the live app and its Git checkout untouched.
sudo mv /srv/apps/fixture-php/.env "$abr_binary_directory/fixture-php.env"
fixture_refused 'app services were not stopped' deploy fixture-php --no-pull
sudo mv "$abr_binary_directory/fixture-php.env" /srv/apps/fixture-php/.env
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'

# Fetch an invalid incoming commit from a disposable local remote; no external
# repository is contacted, and the deployed checkout must remain on its old SHA.
sudo git -c safe.directory=/srv/apps/fixture-php clone --bare --no-hardlinks /srv/apps/fixture-php "$fixture_source/deploy-origin.git"
sudo git clone "$fixture_source/deploy-origin.git" "$fixture_source/deploy-edit"
sudo git -C "$fixture_source/deploy-edit" config user.name 'Abr fixture'
sudo git -C "$fixture_source/deploy-edit" config user.email 'fixture@example.invalid'
sudo git -C "$fixture_source/deploy-edit" rm composer.lock
sudo git -C "$fixture_source/deploy-edit" commit --no-gpg-sign -m 'Missing deployment lock'
sudo git -c safe.directory="$fixture_source/deploy-origin.git" -C "$fixture_source/deploy-edit" push origin main
# The app user needs to traverse the fixture and write its own remote repository.
sudo chmod 755 "$fixture_source"
sudo chown -R abr-fixture-php:abr-fixture-php "$fixture_source/deploy-origin.git"
sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php remote add origin "$fixture_source/deploy-origin.git"
sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php config branch.main.remote origin
sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php config branch.main.merge refs/heads/main
preflight_head=$(sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php rev-parse HEAD)
fixture_refused 'commit composer.lock before deployment' deploy fixture-php
test "$preflight_head" = "$(sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php rev-parse HEAD)"
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
sudo git -C "$fixture_source/deploy-edit" restore --source=HEAD~1 composer.lock
sudo git -C "$fixture_source/deploy-edit" add composer.lock
sudo git -C "$fixture_source/deploy-edit" commit --no-gpg-sign -m 'Restore deployment lock'
sudo git -c safe.directory="$fixture_source/deploy-origin.git" -C "$fixture_source/deploy-edit" push origin main
abr_ci deploy fixture-php
test "$preflight_head" != "$(sudo runuser -u abr-fixture-php -- git -C /srv/apps/fixture-php rev-parse HEAD)"
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'

# Saving settings keeps old services live. Deployment applies new routing and
# reconciles worker units and optional components, retaining port reservations.
sudo cp /var/lib/abr-ci/ports.json "$fixture_source/edit-ports-before.json"
abr_ci edit fixture-php --domain edited.fixture-php.localhost --alias '' --serving-domain '' --queue-workers 2 --scheduler=false
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
abr_ci deploy fixture-php --no-pull
fixture_https edited.fixture-php.localhost | grep -F 'Laravel fixture database=1'
sudo systemctl is-active abr-fixture-php-queue@1.service abr-fixture-php-queue@2.service
if sudo systemctl is-active --quiet abr-fixture-php-scheduler.timer; then
  echo 'Edited scheduler remained active' >&2; exit 1
fi
sudo cmp "$fixture_source/edit-ports-before.json" /var/lib/abr-ci/ports.json
abr_ci edit fixture-php --domain fixture-php.localhost --serving-domain extra.fixture-php.localhost --canonical-host non-www --queue-workers 0 --scheduler=true
abr_ci deploy fixture-php --no-pull
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
if sudo systemctl is-active --quiet abr-fixture-php-queue@2.service; then
  echo 'Disabled queue worker remained active' >&2; exit 1
fi
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
fixture_refused 'import failed; database may be partially changed' database import fixture-php /var/lib/abr-ci/forbidden.sql --yes
printf '\\! touch /var/lib/abr-ci/import-shell-executed\n' | sudo tee /var/lib/abr-ci/shell.sql >/dev/null
fixture_refused 'import failed; database may be partially changed' database import fixture-php /var/lib/abr-ci/shell.sql --yes
sudo test ! -e /var/lib/abr-ci/import-shell-executed
abr_ci enable fixture-php
fixture_https fixture-php.localhost --dump-header "$fixture_source/headers" >/dev/null
fixture_security_headers "$fixture_source/headers"
fixture_missing_assets fixture-php.localhost /build/assets
# PHP-FPM also adds Via on application error responses; strip it there too.
php_missing_status=$(curl --silent --show-error --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -D "$fixture_source/php-missing-headers" -o /dev/null --write-out '%{http_code}' \
  https://fixture-php.localhost/abr-fixture-missing-page)
test "$php_missing_status" = 404
fixture_security_headers "$fixture_source/php-missing-headers"
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/php-config | grep -F '"opcache":"1"'
curl --fail --silent --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/php-config | grep -F '"unprivileged":true,"no_new_privs":true'
fixture_image_cache fixture-php.localhost /srv/apps/fixture-php/public abr-fixture-php
sudo runuser -u abr-fixture-php -- sh -c 'printf "public upload fixture" > /srv/apps/fixture-php/storage/app/public/abr-upload.png'
curl --fail --silent --show-error --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -D "$fixture_source/upload-headers" https://fixture-php.localhost/storage/abr-upload.png -o "$fixture_source/upload-body"
tr -d '\r' < "$fixture_source/upload-headers" | grep -Fix 'Cache-Control: public, max-age=2592000'
test "$(cat "$fixture_source/upload-body")" = 'public upload fixture'
sudo runuser -u abr-fixture-php -- rm /srv/apps/fixture-php/storage/app/public/abr-upload.png
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
# An existing hashed file can still fail to serve; never cache that error.
sudo touch /srv/apps/fixture-php/public/build/assets/denied-AbCd1234.css
sudo chmod 000 /srv/apps/fixture-php/public/build/assets/denied-AbCd1234.css
asset_error_status=$(curl --silent --show-error --insecure --resolve fixture-php.localhost:443:127.0.0.1 \
  -D "$fixture_source/asset-error-headers" -o /dev/null --write-out '%{http_code}' \
  https://fixture-php.localhost/build/assets/denied-AbCd1234.css)
case "$asset_error_status" in
  403|404) ;; # Caddy may reject the file matcher before file_server runs.
  *) echo "Unexpected inaccessible asset status: $asset_error_status" >&2; exit 1 ;;
esac
fixture_security_headers "$fixture_source/asset-error-headers"
if grep -Ei '^cache-control:.*(immutable|max-age=31536000)' "$fixture_source/asset-error-headers"; then
  echo 'Asset error received immutable caching' >&2; exit 1
fi
sudo rm /srv/apps/fixture-php/public/build/assets/denied-AbCd1234.css
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
if grep -Ei '^(server|via|x-powered-by):' "$fixture_source/http-headers"; then echo 'HTTP redirect header leaked' >&2; exit 1; fi
abr_ci restart fixture-php web
abr_ci disable fixture-php
abr_ci enable fixture-php
abr_ci status fixture-php
abr_ci logs fixture-php scheduler

sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-octane
sudo cp /srv/apps/fixture-php/routes/web.php /srv/apps/fixture-octane/routes/web.php
# This app needs built assets during Composer installation: exercise frontend-first.
sudo node -e 'const fs=require("node:fs"); const p="/srv/apps/fixture-octane/package.json"; const c=JSON.parse(fs.readFileSync(p)); c.scripts.prebuild="node abr-fixture-privileges.cjs"; fs.writeFileSync(p,JSON.stringify(c,null,2)+"\n");'
sudo tee /srv/apps/fixture-octane/abr-fixture-assets.php >/dev/null <<'PHP'
<?php
if (!is_file(__DIR__.'/public/build/manifest.json')) {
    fwrite(STDERR, "Composer installation requires built frontend assets\n");
    exit(1);
}
PHP
sudo php -r '$p="/srv/apps/fixture-octane/composer.json"; $c=json_decode(file_get_contents($p),true); $c["scripts"]["pre-install-cmd"][]="@php abr-fixture-assets.php"; file_put_contents($p,json_encode($c,JSON_PRETTY_PRINT|JSON_UNESCAPED_SLASHES)."\n");'
printf '\n.rr.yaml\n' | sudo tee -a /srv/apps/fixture-octane/.gitignore >/dev/null
fixture_git /srv/apps/fixture-octane
abr_ci register --name fixture-octane --type laravel --web-driver octane --domain fixture-octane.localhost --canonical-host www
sudo rm -f /srv/apps/fixture-octane/.env
abr_ci env fixture-octane
sudo test "$(sudo stat -c '%U:%a' /srv/apps/fixture-octane/.env)" = abr-fixture-octane:600
sudo grep -Fx 'APP_ENV=production' /srv/apps/fixture-octane/.env
sudo grep -Fx 'APP_DEBUG=false' /srv/apps/fixture-octane/.env
abr_ci deploy fixture-octane --no-pull
fixture_https www.fixture-octane.localhost -D "$fixture_source/octane-headers" | grep -F 'Laravel fixture database=1'
fixture_security_headers "$fixture_source/octane-headers"
fixture_image_cache www.fixture-octane.localhost /srv/apps/fixture-octane/public abr-fixture-octane
fixture_redirect www.fixture-octane.localhost www.fixture-octane.localhost http
fixture_redirect fixture-octane.localhost www.fixture-octane.localhost
sudo test ! -f /srv/apps/fixture-octane/rr
sudo test -x /usr/local/bin/rr
abr_ci restart fixture-octane web
# Exercise Caddy's error route while the disposable upstream is unavailable.
sudo systemctl stop abr-fixture-octane-octane.service
fixture_refused 'expected listener is missing' doctor
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

nuxt_fixture="$fixture_source/nuxt"
mkdir -p "$nuxt_fixture"
cat > "$nuxt_fixture/package.json" <<'JSON'
{"name":"abr-nuxt-fixture","private":true,"type":"module","scripts":{"prebuild":"node abr-fixture-privileges.cjs","build":"nuxt build","postinstall":"nuxt prepare"},"dependencies":{"nuxt":"4.6.0","vue":"3.5.43","vue-router":"5.4.0"},"overrides":{"simple-git":"4.0.2"}}
JSON
# Resolve/audit the identical dependencies once for both rendering modes.
npm --prefix "$nuxt_fixture" install --package-lock-only --ignore-scripts --allow-remote=all --no-fund
# braces/node-forge have no published patches; keep their high findings visible.
npm --prefix "$nuxt_fixture" audit --audit-level=critical
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
  cp "$nuxt_fixture/package.json" "$nuxt_fixture/package-lock.json" "$nuxt_source/"
  printf 'export default defineNuxtConfig({ssr: %s, devtools: {enabled: false}})\n' "$rendering" | tee "$nuxt_source/nuxt.config.ts" >/dev/null
  printf '<template><h1>Abr Nuxt fixture</h1></template>\n' | tee "$nuxt_source/app/app.vue" >/dev/null
  printf 'node_modules\n.output\n.nuxt\n.env\n' | tee "$nuxt_source/.gitignore" >/dev/null
  cp "$fixture_source/laravel/abr-fixture-privileges.cjs" "$nuxt_source/"
  sudo cp -R "$nuxt_source" "$dir"
  fixture_git "$dir"
  abr_ci register --name "$app" --type nuxt --domain "$app_domain"
  abr_ci env "$app"
  sudo test ! -e "$dir/.env"
  sudo test ! -e "/var/lib/abr-ci/databases/$app.json"
  sudo test ! -e "/var/lib/abr-ci/credentials/$app.env"
  abr_ci deploy "$app" --no-pull
  sudo test ! -e "$dir/.env"
  sudo test ! -e "/etc/php/8.5/fpm/pool.d/abr-$app.conf"
  for component in octane queue@1 scheduler nightwatch inertia-ssr; do
    sudo test ! -e "/etc/systemd/system/abr-$app-$component.service"
  done
  fixture_caddy_format "/etc/caddy/abr.d/abr-$app.caddy"
  test "$(sudo systemctl show "abr-$app-nuxt.service" --property=NoNewPrivileges --value)" = yes
  nuxt_pid=$(sudo systemctl show "abr-$app-nuxt.service" --property=MainPID --value)
  sudo awk '/^NoNewPrivs:/ {if ($2 != 1) exit 1; found=1} END {if (!found) exit 1}' "/proc/$nuxt_pid/status"
  fixture_redirect "$app_domain" "$app_domain" http
  fixture_https "$app_domain" -D "$fixture_source/nuxt-page-headers" -o "$fixture_source/$app.html"
  fixture_security_headers "$fixture_source/nuxt-page-headers"
  fixture_image_cache "$app_domain" "$dir/.output/public" "abr-$app"
  if [[ $rendering == true ]]; then grep -Fq 'Abr Nuxt fixture' "$fixture_source/$app.html"; else grep -Fq '__nuxt' "$fixture_source/$app.html"; fi
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
# Full backup and latest-code restore exercise the same real Ubuntu services.
source "$abr_smoke_script_directory/full-transfer-smoke.sh"
abr_ci ports
# Real services own their occupied ports, including RoadRunner's child processes.
abr_ci doctor > "$fixture_source/doctor-healthy.log"
grep -F 'listening, owned by active abr-fixture-octane-octane.service' "$fixture_source/doctor-healthy.log"
grep -F 'listening, owned by active abr-fixture-spa-nuxt.service' "$fixture_source/doctor-healthy.log"
grep -F 'app endpoint checks passed' "$fixture_source/doctor-healthy.log"
# A foreign listener on another loopback address must fail even while the app
# remains active and owns its normal listener on the same reserved port.
doctor_port=$(abr_ci ports | awk '$1 == "fixture-spa" && $2 == "nuxt-http" {print $3}')
test -n "$doctor_port"
sudo systemd-run --unit=abr-ci-foreign-listener --collect /usr/bin/python3 -c \
  'import pathlib,socket,sys,time; s=socket.socket(); s.bind(("127.0.0.2",int(sys.argv[1]))); s.listen(); pathlib.Path(sys.argv[2]).touch(); time.sleep(3600)' \
  "$doctor_port" "$fixture_source/doctor-listener-ready"
for attempt in {1..50}; do
  if sudo test -f "$fixture_source/doctor-listener-ready"; then break; fi
  sleep 0.1
done
sudo test -f "$fixture_source/doctor-listener-ready"
fixture_refused 'port conflict: listener PID' doctor
sudo systemctl stop abr-ci-foreign-listener.service
abr_ci disable fixture-spa
abr_ci doctor > "$fixture_source/doctor-disabled.log"
grep -F "fixture-spa/nuxt-http at $doctor_port: available (app disabled)" "$fixture_source/doctor-disabled.log"
abr_ci enable fixture-spa
abr_ci doctor
# File-log maintenance on real app-owned Laravel/Nuxt paths. Clearing retains
# the inode, permissions, open writers, uploads, history and shared journal.
sudo runuser -u abr-fixture-php -- mkdir -p /srv/apps/fixture-php/storage/logs
sudo runuser -u abr-fixture-spa -- mkdir -p /srv/apps/fixture-spa/logs
sudo runuser -u abr-fixture-php -- sh -c 'printf "Laravel log fixture\n" > "$1"; printf "Keep uploaded log\n" > "$2"' sh \
  /srv/apps/fixture-php/storage/logs/abr-clear.log /srv/apps/fixture-php/storage/app/public/abr-upload.log
sudo runuser -u abr-fixture-spa -- sh -c 'printf "Nuxt log fixture\n" > "$1"' sh /srv/apps/fixture-spa/logs/abr-clear.log
printf 'Deployment log fixture\n' | sudo tee /var/lib/abr-ci/deployments/fixture-php/abr-clear.log >/dev/null
printf '{"fixture":"keep history"}\n' | sudo tee /var/lib/abr-ci/deployments/fixture-php/abr-clear.json >/dev/null
sudo logger -t abr-log-clear-fixture 'journal preserved'
fixture_log_identity=$(sudo stat -c '%i:%u:%g:%a' /srv/apps/fixture-php/storage/logs/abr-clear.log)
fixture_refused 'use --yes to confirm' logs clear fixture-php
abr_ci logs clear fixture-php --dry-run
sudo test -s /srv/apps/fixture-php/storage/logs/abr-clear.log
abr_ci logs clear fixture-php --type application --yes
sudo test ! -s /srv/apps/fixture-php/storage/logs/abr-clear.log
test "$(sudo stat -c '%i:%u:%g:%a' /srv/apps/fixture-php/storage/logs/abr-clear.log)" = "$fixture_log_identity"
sudo test -s /srv/apps/fixture-spa/logs/abr-clear.log
sudo test -s /var/lib/abr-ci/deployments/fixture-php/abr-clear.log
# Keep an actual runtime-user writer open across truncation.
sudo runuser -u abr-fixture-php -- bash -c '
  exec 3>>"$1"
  touch "$2"
  while [[ ! -e $3 ]]; do sleep 0.1; done
  printf "Writer survived\n" >&3
' bash /srv/apps/fixture-php/storage/logs/abr-clear.log \
  /srv/apps/fixture-php/storage/logs/writer-ready /srv/apps/fixture-php/storage/logs/writer-go &
fixture_writer_pid=$!
for fixture_writer_attempt in $(seq 1 50); do
  if sudo test -e /srv/apps/fixture-php/storage/logs/writer-ready; then break; fi
  sleep 0.1
done
sudo test -e /srv/apps/fixture-php/storage/logs/writer-ready
abr_ci logs clear --all --yes
sudo test ! -s /srv/apps/fixture-spa/logs/abr-clear.log
sudo test ! -s /var/lib/abr-ci/deployments/fixture-php/abr-clear.log
sudo touch /srv/apps/fixture-php/storage/logs/writer-go
wait "$fixture_writer_pid"
sudo grep -Fx 'Writer survived' /srv/apps/fixture-php/storage/logs/abr-clear.log
sudo test -s /srv/apps/fixture-php/storage/app/public/abr-upload.log
sudo test -s /var/lib/abr-ci/deployments/fixture-php/abr-clear.json
sudo test -s /srv/apps/fixture-php/.env
sudo journalctl --no-pager -t abr-log-clear-fixture | grep -F 'journal preserved'
sudo rm /srv/apps/fixture-php/storage/logs/writer-ready /srv/apps/fixture-php/storage/logs/writer-go
# Refuse a same-filesystem bind mount so external data cannot be truncated.
mkdir -p "$fixture_source/external-logs"
printf 'Keep external log\n' > "$fixture_source/external-logs/external.log"
sudo mkdir /srv/apps/fixture-php/storage/logs/mounted
sudo mount --bind "$fixture_source/external-logs" /srv/apps/fixture-php/storage/logs/mounted
fixture_refused 'before clearing logs' logs clear fixture-php --type application --yes
test -s "$fixture_source/external-logs/external.log"
sudo umount /srv/apps/fixture-php/storage/logs/mounted
sudo rmdir /srv/apps/fixture-php/storage/logs/mounted
# Exercise native GNU du, live statfs and one metadata query on the real hosts.
abr_ci disk --refresh --json > "$fixture_source/disk-fresh.json"
abr_ci disk --json > "$fixture_source/disk-cached.json"
abr_ci disk --filesystem-only --json > "$fixture_source/disk-filesystems.json"
python3 - "$fixture_source" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
fresh = json.loads((root / "disk-fresh.json").read_text())
cached = json.loads((root / "disk-cached.json").read_text())
filesystems = json.loads((root / "disk-filesystems.json").read_text())
assert not fresh["cached"] and cached["cached"]
assert fresh["measured_at"] == cached["measured_at"]
assert fresh["total_estimated_bytes"] == cached["total_estimated_bytes"]
apps = {a["name"]: a for a in fresh["apps"]}
assert set(apps) == {"fixture-php", "fixture-octane", "fixture-spa", "fixture-ssr"}
assert apps["fixture-php"]["database_estimated_bytes"] > 0
assert apps["fixture-php"]["home_bytes"] > 0
assert not apps["fixture-spa"]["database_managed"]
assert fresh["files_bytes"] == sum(a["files_bytes"] for a in apps.values())
assert fresh["total_estimated_bytes"] == fresh["files_bytes"] + fresh["database_estimated_bytes"]
assert not filesystems["apps"]
for report in (fresh, cached, filesystems):
    assert report["filesystems"]
    for fs in report["filesystems"]:
        assert fs["capacity_bytes"] > 0
        assert 0 <= fs["available_bytes"] <= fs["capacity_bytes"]
PY
# An empty 1 GiB sparse file should not add 1 GiB of actual allocated space.
sudo runuser -u abr-fixture-php -- truncate -s 1G /srv/apps/fixture-php/storage/logs/abr-sparse.bin
abr_ci disk fixture-php --refresh --json > "$fixture_source/disk-single.json"
python3 - "$fixture_source" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
all_apps = json.loads((root / "disk-fresh.json").read_text())["apps"]
single = json.loads((root / "disk-single.json").read_text())
assert len(single["apps"]) == 1 and single["apps"][0]["name"] == "fixture-php"
before = next(a["project_bytes"] for a in all_apps if a["name"] == "fixture-php")
assert single["apps"][0]["project_bytes"] - before < 32 * 1024 * 1024
PY
sudo runuser -u abr-fixture-php -- rm /srv/apps/fixture-php/storage/logs/abr-sparse.bin
# Real file readers and a combined journal query without any persisted log copy.
sudo runuser -u abr-fixture-php -- sh -c 'printf "Old reader fixture\nLaravel reader fixture\n" > "$1"' sh /srv/apps/fixture-php/storage/logs/abr-reader.log
sudo runuser -u abr-fixture-spa -- sh -c 'printf "Nuxt reader fixture\n" > "$1"' sh /srv/apps/fixture-spa/logs/abr-reader.log
printf 'Deployment reader fixture\n' | sudo tee /var/lib/abr-ci/deployments/fixture-php/abr-reader.log >/dev/null
abr_ci logs fixture-php --type application --lines 1 > "$fixture_source/log-single.txt"
grep -F 'Laravel reader fixture' "$fixture_source/log-single.txt"
if grep -F 'Old reader fixture' "$fixture_source/log-single.txt"; then
  echo 'Log reader ignored its line limit' >&2; exit 1
fi
abr_ci logs --all --type application > "$fixture_source/log-all.txt"
grep -F '== fixture-php / application logs ==' "$fixture_source/log-all.txt"
grep -F '== fixture-spa / application logs ==' "$fixture_source/log-all.txt"
grep -F 'Laravel reader fixture' "$fixture_source/log-all.txt"
grep -F 'Nuxt reader fixture' "$fixture_source/log-all.txt"
abr_ci logs --all --type deployment > "$fixture_source/log-deployments.txt"
grep -F 'Deployment reader fixture' "$fixture_source/log-deployments.txt"
abr_ci logs --all --lines 20 > "$fixture_source/log-journals.txt"
grep -F 'Read recent service journals' "$fixture_source/log-journals.txt"
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
sudo getfacl -pcn /var/lib/abr-ci/git/id_ed25519 | grep -Fx "user:$removed_git_uid:---"
sudo test -f /srv/apps/fixture-php/.env
sudo test -f /var/lib/abr-ci/credentials/fixture-php.env
sudo test "$(sudo stat -c '%U' /srv/apps/fixture-php/.env)" = root
if getent passwd abr-fixture-php; then echo 'Managed user was not removed' >&2; exit 1; fi
# Reuse retained data/credentials and verify a repeated deployment stays clean.
abr_ci register --name fixture-php --type laravel --domain fixture-php.localhost --scheduler
abr_ci deploy fixture-php --no-pull
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
fixture_refused 'use --purge --yes to confirm' remove fixture-php --purge
abr_ci remove fixture-php --purge --dry-run
sudo test -f /srv/apps/fixture-php/.env
# Bind mounts must not cause deletion of storage outside the registered project.
mkdir -p "$fixture_source/mounted-uploads"
touch "$fixture_source/mounted-uploads/keep"
sudo mkdir -p /srv/apps/fixture-php/storage/abr-mount-fixture
sudo mount --bind "$fixture_source/mounted-uploads" /srv/apps/fixture-php/storage/abr-mount-fixture
fixture_refused 'before full removal' remove fixture-php --purge --yes
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
SELECT (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='fixture_php') + (SELECT COUNT(*) FROM mysql.user WHERE User='fixture_php' AND Host IN ('localhost','127.0.0.1'));
SQL
# Ordinary removals and foreign databases must still retain their data.
sudo test -f /srv/apps/fixture-octane/.env
sudo test -f /var/lib/abr-ci/git/id_ed25519
for path in /var/lib/abr-ci /var/lib/abr-ci/git/id_ed25519 /var/lib/abr-ci/composer/auth.json; do
  if sudo getfacl -pcn "$path" | grep -E "^user:$purged_git_uid:"; then
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
# Repeated setup must allow its recorded loopback account, retain its password,
# and leave other apps' retained databases intact.
abr_ci setup --hostname abr-ci-vps --no-firewall --ssh-port 22 --admin-user root
fixture_shell
sudo cmp /root/.zshrc.pre-abr "$abr_binary_directory/zshrc-before"
sudo cmp /var/lib/abr-ci/mysql-admin.json "$abr_binary_directory/admin-before.json"
sudo mysql --protocol=socket --user=root --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='fixture_octane';" | grep -Fx 1
sudo rm "$abr_binary_directory/admin.cnf" "$abr_binary_directory/admin-before.json"
echo 'Disposable host smoke test passed.'
