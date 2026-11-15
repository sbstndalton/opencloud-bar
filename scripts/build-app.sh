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
# ARCHS="arm64 x86_64" (the default) makes a universal app: Apple silicon and Intel Macs.
# Each architecture is built on its own and joined with lipo: `swift build --arch a --arch b`
# needs the newer build system, which has a plist parsing bug in current toolchains.
ARCHS="${ARCHS:-arm64 x86_64}"
# --build-system native avoids that bug; older toolchains lack the flag.
native=()
if swift build --help 2>&1 | grep -q -- "--build-system"; then native=(--build-system native); fi
slices=()
for arch in $ARCHS; do
  triple="$arch-apple-macosx14.0"
  swift build -c release "${native[@]}" --triple "$triple"
  slices+=("$(swift build -c release "${native[@]}" --triple "$triple" --show-bin-path)/OpenCloudBar")
done
APP="build/OpenCloud Bar.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create "${slices[@]}" -output "$APP/Contents/MacOS/OpenCloudBar"
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
