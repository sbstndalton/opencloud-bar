#!/bin/bash
# Builds "OpenCloud Bar.app" into ./build (release, ad-hoc signed).
set -euo pipefail
cd "$(dirname "$0")/.."
# --build-system native avoids a plist parsing bug in newer toolchains; older ones lack the flag.
if swift build --help 2>&1 | grep -q -- "--build-system"; then
  swift build -c release --build-system native
else
  swift build -c release
fi
APP="build/OpenCloud Bar.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp .build/release/OpenCloudBar "$APP/Contents/MacOS/OpenCloudBar"
cp Resources/Info.plist "$APP/Contents/Info.plist"
# Bundle rclone if a folder holding it is given (RCLONE_DIR=... ./scripts/build-app.sh).
if [ -n "${RCLONE_DIR:-}" ]; then
  mkdir -p "$APP/Contents/Helpers"
  cp "$RCLONE_DIR/rclone" "$APP/Contents/Helpers/rclone"
  cp "$RCLONE_DIR/rclone-LICENSE.txt" "$APP/Contents/Resources/rclone-LICENSE.txt"
  codesign --force --sign - "$APP/Contents/Helpers/rclone"
fi
codesign --force --sign - "$APP"
echo "built $APP"
