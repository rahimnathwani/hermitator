#!/bin/sh
# Build an agent base image with podman and export it as a plain directory.
#
#   build-image.sh [name] [context-dir]
#
# The result lands in $HERMITATOR_DATA_DIR/images/<name>-<YYYYMMDD[.N]>/rootfs
# and the symlink images/<name> is pointed at it. New agents resolve the
# symlink once and pin the dated directory, because an OverlayFS upper layer
# must never see its lower layer change underneath it.
set -eu

name=${1:-universal}
context=${2:-$(dirname "$0")/../image}
data=${HERMITATOR_DATA_DIR:-/var/lib/hermitator}
images=$data/images

stamp=$(date -u +%Y%m%d)
version=$stamp
n=1
while [ -e "$images/$name-$version" ]; do
	version=$stamp.$n
	n=$((n + 1))
done
dest=$images/$name-$version
tag=localhost/hermitator-$name:$version

echo "building $tag from $context"
# Host networking keeps the build off podman's bridge, whose default
# subnet is easy to clash with.
podman build --pull=newer --network=host --tag "$tag" "$context"

tmp=$dest.partial
rm -rf "$tmp"
mkdir -p "$tmp/rootfs"
cid=$(podman create "$tag")
trap 'podman rm -f "$cid" >/dev/null 2>&1 || true' EXIT
podman export "$cid" | tar -C "$tmp/rootfs" --numeric-owner -xpf -

# podman leaves build-time networking files behind; give agents sane ones.
printf 'nameserver 1.1.1.1\nnameserver 9.9.9.9\noptions edns0\n' >"$tmp/rootfs/etc/resolv.conf"
printf '127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n' >"$tmp/rootfs/etc/hosts"
echo agent >"$tmp/rootfs/etc/hostname"

# Exported filesystems lose the image's ENV; hermitator reads it from here.
podman image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$tag" | sed '/^$/d' >"$tmp/ENV"
printf '%s\n' "$tag" >"$tmp/IMAGE"
du -sh "$tmp/rootfs" | cut -f1 >"$tmp/SIZE"
mv "$tmp" "$dest"
ln -sfn "$name-$version" "$images/$name"
echo "exported $dest ($(cat "$dest/SIZE"))"
