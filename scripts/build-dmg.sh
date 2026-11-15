#!/bin/bash
# Builds build/OpenCloud-Bar.dmg with rclone inside the app: drag the app to Applications.
set -euo pipefail
cd "$(dirname "$0")/.."
RCLONE_DIR="$(mktemp -d)"
./scripts/fetch-rclone.sh osx arm64 "$RCLONE_DIR"
RCLONE_DIR="$RCLONE_DIR" ./scripts/build-app.sh
STAGE="$(mktemp -d)"
cp -R "build/OpenCloud Bar.app" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f build/OpenCloud-Bar.dmg
# hdiutil sometimes fails with "Resource busy" on CI machines; retry a few times.
for i in 1 2 3 4 5; do
  hdiutil create -volname "OpenCloud Bar" -srcfolder "$STAGE" -ov -format UDZO build/OpenCloud-Bar.dmg && break
  [ "$i" = 5 ] && exit 1
  sleep $((i * 5))
done
echo "built build/OpenCloud-Bar.dmg"
