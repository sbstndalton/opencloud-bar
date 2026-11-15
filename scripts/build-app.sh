#!/bin/bash
# Builds "OpenCloud Bar.app" into ./build (release, ad-hoc signed).
# VERSION=1.2.3 ./scripts/build-app.sh stamps the version into the app (default 0.0.0-dev,
# which the app treats as a development build: no update offers).
set -euo pipefail
VERSION="${VERSION:-0.0.0-dev}"
VERSION="${VERSION#v}"
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "VERSION must look like 1.2.3 or 1.2.3-rc.1, not '$VERSION'" >&2
  exit 1
fi
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
# The source plist stays as it is; only the built app gets the version. CFBundleVersion must be
# numbers and dots, so it drops any pre-release part.
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion ${VERSION%%-*}" "$APP/Contents/Info.plist"
cp packaging/icon/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
# Bundle rclone if a folder holding it is given (RCLONE_DIR=... ./scripts/build-app.sh).
if [ -n "${RCLONE_DIR:-}" ]; then
  mkdir -p "$APP/Contents/Helpers"
  cp "$RCLONE_DIR/rclone" "$APP/Contents/Helpers/rclone"
  cp "$RCLONE_DIR/rclone-LICENSE.txt" "$APP/Contents/Resources/rclone-LICENSE.txt"
  codesign --force --sign - "$APP/Contents/Helpers/rclone"
fi
codesign --force --sign - "$APP"
echo "built $APP (version $VERSION)"
