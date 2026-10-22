#!/bin/sh
# Build hermitator for Linux on this machine and install it on a host.
#
#   deploy/deploy.sh [user@]host [domain]
#
# The host needs SSH access as root (or a user with passwordless sudo).
set -eu

host=${1:?usage: deploy/deploy.sh [user@]host [domain]}
domain=${2:-}
arch=${GOARCH:-amd64}
cd "$(dirname "$0")/.."

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "==> building $version for linux/$arch"
for cmd in hermitator hermitator-guest; do
	GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$out/bin/$cmd" "./cmd/$cmd"
done
cp -a deploy image "$out/"

echo "==> copying to $host"
ssh "$host" 'mkdir -p /usr/local/src/hermitator'
rsync -az --delete "$out/" "$host:/usr/local/src/hermitator/"

echo "==> installing"
sudo=""
case "$host" in root@*) ;; *@*) sudo="sudo" ;; esac
ssh "$host" "$sudo env HERMITATOR_DOMAIN='$domain' HERMITATOR_PREVIEW_DOMAIN='${PREVIEW_DOMAIN:-}' WITH_WHISPER='${WITH_WHISPER:-}' WITH_TTS='${WITH_TTS:-}' sh /usr/local/src/hermitator/deploy/install.sh"
