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
hdiutil create -quiet -volname "OpenCloud Bar" -srcfolder "$STAGE" -ov -format UDZO build/OpenCloud-Bar.dmg
echo "built build/OpenCloud-Bar.dmg"
