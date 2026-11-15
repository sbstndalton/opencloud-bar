#!/usr/bin/env bash
# Downloads the current rclone release for OS/ARCH, checks it against rclone's SHA256SUMS,
# and puts the binary (and its license) in DEST.   usage: fetch-rclone.sh osx|windows|linux amd64|arm64 DEST
set -euo pipefail
os=$1 arch=$2 dest=$3
ver=$(curl -fsSL https://downloads.rclone.org/version.txt | awk '{print $2}')
zip="rclone-$ver-$os-$arch.zip"
tmp=$(mktemp -d)
curl -fsSL -o "$tmp/$zip" "https://downloads.rclone.org/$ver/$zip"
curl -fsSL -o "$tmp/SHA256SUMS" "https://downloads.rclone.org/$ver/SHA256SUMS"
want=$(grep " $zip\$" "$tmp/SHA256SUMS" | awk '{print $1}')
have=$( (sha256sum "$tmp/$zip" 2>/dev/null || shasum -a 256 "$tmp/$zip") | awk '{print $1}')
[ -n "$want" ] && [ "$want" = "$have" ] || { echo "checksum mismatch for $zip" >&2; exit 1; }
unzip -q "$tmp/$zip" -d "$tmp"
mkdir -p "$dest"
bin=rclone; [ "$os" = windows ] && bin=rclone.exe
cp "$tmp/rclone-$ver-$os-$arch/$bin" "$dest/"
cp "$tmp/rclone-$ver-$os-$arch/README.txt" "$dest/rclone-README.txt" 2>/dev/null || true
curl -fsSL -o "$dest/rclone-LICENSE.txt" "https://raw.githubusercontent.com/rclone/rclone/$ver/COPYING"
chmod +x "$dest/$bin"
echo "rclone $ver -> $dest/$bin"
