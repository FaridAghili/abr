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

# GitHub runner images ship MySQL with this documented test password and other
# inactive web servers. These adjustments belong only to the disposable fixture.
if [[ ${GITHUB_ACTIONS:-false} == true ]]; then
  sudo systemctl mask --now nginx apache2
  printf '[client]\nuser=root\npassword=root\n' | sudo tee /root/.my.cnf >/dev/null
  sudo chmod 600 /root/.my.cnf
fi
sites_ci setup --no-firewall --ssh-port 22

fixture_source=$(mktemp -d)
trap 'rm -rf "$fixture_source"' EXIT
composer create-project --no-install --no-scripts --prefer-dist 'laravel/laravel:^13.0' "$fixture_source/laravel"
(
  cd "$fixture_source/laravel"
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
curl --fail --silent --show-error --insecure --resolve fixture-php.localhost:443:127.0.0.1 https://fixture-php.localhost/ | rg 'Laravel fixture database=1'
sudo test -S /run/php/sites-fixture-php.sock
sudo test "$(stat -c '%a' /srv/apps/fixture-php/.env)" = 600
sudo test "$(stat -c '%a' /var/lib/sites-ci/credentials/fixture-php.env)" = 600
sites_ci restart fixture-php web
sites_ci disable fixture-php
sites_ci enable fixture-php
sites_ci status fixture-php
sites_ci logs fixture-php scheduler

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
  curl --fail --silent --show-error --insecure --resolve "$app.localhost:443:127.0.0.1" "https://$app.localhost/" -o "$fixture_source/$app.html"
  if [[ $rendering == true ]]; then rg 'Sites Nuxt fixture' "$fixture_source/$app.html"; else rg '__nuxt' "$fixture_source/$app.html"; fi
  sites_ci restart "$app" web
done
sites_ci ports
sites_ci remove fixture-ssr
sites_ci remove fixture-spa
sites_ci remove fixture-php
sudo test -f /srv/apps/fixture-php/.env
sudo test -f /var/lib/sites-ci/credentials/fixture-php.env
if getent passwd sites-fixture-php; then echo 'Managed user was not removed' >&2; exit 1; fi
sites_ci doctor
echo 'Disposable host smoke test passed.'
