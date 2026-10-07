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
sites_ci setup --no-firewall --ssh-port 22 --admin-user root
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
  npm install --package-lock-only --ignore-scripts
)
sudo cp -R "$fixture_source/laravel" /srv/apps/fixture-php
sudo tee /srv/apps/fixture-php/routes/web.php >/dev/null <<'PHP'
<?php
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route;
Route::get('/', fn () => 'Laravel fixture database='.DB::select('SELECT 1 AS ok')[0]->ok);
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
sudo runuser -u sites-fixture-php -- test -r /var/lib/sites-ci/git/id_ed25519
sudo runuser -u sites-fixture-php -- ssh-keygen -y -P '' -f /var/lib/sites-ci/git/id_ed25519 >/dev/null
sites_ci git setup
sudo runuser -u sites-fixture-php -- test ! -r /var/lib/sites-ci/credentials/fixture-php.env
sudo runuser -u nobody -- test ! -r /var/lib/sites-ci/git/id_ed25519
fixture_https fixture-php.localhost | grep -F 'Laravel fixture database=1'
sudo test -S /run/php/sites-fixture-php.sock
sudo test "$(sudo stat -c '%a' /srv/apps/fixture-php/.env)" = 600
sudo test "$(sudo stat -c '%a' /var/lib/sites-ci/credentials/fixture-php.env)" = 600
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
  sudo npm --prefix "$dir" install --package-lock-only --ignore-scripts
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
