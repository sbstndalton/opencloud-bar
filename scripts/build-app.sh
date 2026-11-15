#!/bin/bash
# Builds "OpenCloud Bar.app" into ./build (release, ad-hoc signed).
set -euo pipefail
cd "$(dirname "$0")/.."
swift build -c release --build-system native
APP="build/OpenCloud Bar.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp .build/release/OpenCloudBar "$APP/Contents/MacOS/OpenCloudBar"
cp Resources/Info.plist "$APP/Contents/Info.plist"
codesign --force --sign - "$APP"
echo "built $APP"
