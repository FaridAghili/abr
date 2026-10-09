#!/usr/bin/env bash
set -euo pipefail

if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  echo 'Abr requires Linux AMD64; run this installer on your Ubuntu VPS.' >&2
  exit 1
fi

abr_release_url=$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --head --output /dev/null --write-out '%{url_effective}' https://github.com/FaridAghili/abr/releases/latest)
abr_release_tag=${abr_release_url##*/}
if [[ ! $abr_release_tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'Could not resolve the latest stable Abr release.' >&2
  exit 1
fi

abr_install_directory=$(mktemp -d)
trap 'rm -rf -- "$abr_install_directory"' EXIT
abr_download_url="https://github.com/FaridAghili/abr/releases/download/$abr_release_tag"
for abr_asset in abr-linux-amd64 SHA256SUMS; do
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    "$abr_download_url/$abr_asset" --output "$abr_install_directory/$abr_asset"
done
(
  cd "$abr_install_directory"
  sha256sum --check --strict --ignore-missing SHA256SUMS
)

if [[ $EUID == 0 ]]; then
  install -o root -g root -m 755 "$abr_install_directory/abr-linux-amd64" /usr/local/bin/abr
else
  sudo install -o root -g root -m 755 "$abr_install_directory/abr-linux-amd64" /usr/local/bin/abr
fi
printf 'Installed Abr %s to /usr/local/bin/abr. Next: abr setup --dry-run\n' "$abr_release_tag"
