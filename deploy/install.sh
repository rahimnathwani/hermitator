#!/bin/sh
# Install or upgrade hermitator on a Linux host. Run it as root from an
# unpacked release (or let deploy/deploy.sh copy one over and run it):
#
#   bin/hermitator  bin/hermitator-guest  deploy/  image/
#
# Optional environment:
#   HERMITATOR_DOMAIN   serve the panel on https://DOMAIN through Caddy
#   GVISOR_RELEASE      gVisor release to install (default below)
#   WITH_WHISPER=1      build whisper.cpp for voice note transcription
#
# Safe to run again: it only changes what is missing or out of date.
set -eu

GVISOR_RELEASE=${GVISOR_RELEASE:-20261012.0}
DOMAIN=${HERMITATOR_DOMAIN:-}
src=$(cd "$(dirname "$0")/.." && pwd)
data=/var/lib/hermitator
envfile=/etc/hermitator/hermitator.env

say() { printf '==> %s\n' "$*"; }

[ "$(id -u)" = 0 ] || { echo "install.sh must run as root" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || [ "$(uname -m)" = aarch64 ] || { echo "unsupported architecture $(uname -m)" >&2; exit 1; }

say "packages"
if command -v pacman >/dev/null 2>&1; then
	pacman -S --needed --noconfirm iproute2 nftables podman ffmpeg sqlite curl ${DOMAIN:+caddy} 2>&1 | grep -v 'is up to date' || true
elif command -v apt-get >/dev/null 2>&1; then
	apt-get update -qq
	apt-get install -y -qq iproute2 nftables podman ffmpeg sqlite3 curl zstd ${DOMAIN:+caddy} >/dev/null
else
	echo "install iproute2, nftables, podman, ffmpeg and curl yourself, then rerun" >&2
fi

say "gVisor $GVISOR_RELEASE"
gv=/opt/gvisor/$GVISOR_RELEASE
if ! "$gv/runsc" --version >/dev/null 2>&1; then
	tmp=$(mktemp -d)
	url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$(uname -m)
	curl -fsSL -o "$tmp/gvisor.tar.zstd" "$url/gvisor.tar.zstd"
	curl -fsSL -o "$tmp/gvisor.tar.zstd.sha512" "$url/gvisor.tar.zstd.sha512"
	(cd "$tmp" && sha512sum -c gvisor.tar.zstd.sha512 >/dev/null)
	mkdir -p "$gv"
	tar --zstd -xf "$tmp/gvisor.tar.zstd" -C "$tmp"
	cp -a "$tmp/runsc" "$tmp/gvisor-bin" "$gv/"
	rm -rf "$tmp"
fi
ln -sfn "$gv" /opt/gvisor/current
# runsc finds its sidecars in gvisor-bin/ next to the resolved binary.
ln -sfn /opt/gvisor/current/runsc /usr/local/bin/runsc

say "binaries"
install -m 0755 "$src/bin/hermitator" /usr/local/bin/hermitator
install -m 0755 "$src/bin/hermitator-guest" /usr/local/bin/hermitator-guest
mkdir -p /usr/local/share/hermitator
rm -rf /usr/local/share/hermitator/image /usr/local/share/hermitator/deploy
cp -a "$src/image" "$src/deploy" /usr/local/share/hermitator/

say "configuration"
mkdir -p /etc/hermitator "$data"
chmod 0700 /etc/hermitator "$data"
if [ ! -f "$envfile" ]; then
	url=http://127.0.0.1:8080
	[ -n "$DOMAIN" ] && url=https://$DOMAIN
	umask 077
	cat >"$envfile" <<CONF
# hermitator configuration. Keep this file secret: the master key decrypts
# every stored credential, and losing it loses them.
HERMITATOR_MASTER_KEY=$(/usr/local/bin/hermitator keygen)
HERMITATOR_PUBLIC_URL=$url
HERMITATOR_LISTEN=127.0.0.1:8080
HERMITATOR_DATA_DIR=$data
CONF
	say "wrote $envfile with a new master key"
fi

if [ -n "$DOMAIN" ]; then
	say "Caddy for $DOMAIN"
	mkdir -p /etc/caddy
	if ! grep -q "^$DOMAIN " /etc/caddy/Caddyfile 2>/dev/null; then
		cat >/etc/caddy/Caddyfile <<CADDY
# Managed by hermitator's install.sh.
$DOMAIN {
	reverse_proxy 127.0.0.1:8080
}
CADDY
	fi
	systemctl enable --now caddy >/dev/null 2>&1
	systemctl reload caddy
fi

if [ "${WITH_WHISPER:-}" = 1 ] && [ ! -x /usr/local/bin/whisper-cli ]; then
	say "whisper.cpp"
	command -v cmake >/dev/null 2>&1 || { command -v pacman >/dev/null 2>&1 && pacman -S --needed --noconfirm cmake base-devel git >/dev/null; } || apt-get install -y -qq cmake build-essential git >/dev/null
	mkdir -p /usr/local/src && cd /usr/local/src
	[ -d whisper.cpp ] || git clone --depth 1 https://github.com/ggml-org/whisper.cpp
	cmake -S whisper.cpp -B whisper.cpp/build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF >/dev/null
	cmake --build whisper.cpp/build -j "$(nproc)" --target whisper-cli >/dev/null
	install -m 0755 whisper.cpp/build/bin/whisper-cli /usr/local/bin/whisper-cli
	mkdir -p "$data/whisper"
	[ -f "$data/whisper/ggml-small.bin" ] || sh whisper.cpp/models/download-ggml-model.sh small "$data/whisper" >/dev/null
fi

if [ ! -e "$data/images/universal" ]; then
	say "building the universal agent image (this takes a while)"
	HERMITATOR_DATA_DIR=$data sh /usr/local/share/hermitator/deploy/build-image.sh universal /usr/local/share/hermitator/image
fi

say "service"
install -m 0644 "$src/deploy/hermitator.service" /etc/systemd/system/hermitator.service
systemctl daemon-reload
systemctl enable hermitator >/dev/null 2>&1
systemctl restart hermitator
sleep 1
systemctl --no-pager --lines=0 status hermitator | head -3

if ! hermitator user list 2>/dev/null | grep -q ' admin '; then
	say "no admin yet; create one with: hermitator user add you@example.com --admin"
fi
say "done"
