#!/bin/bash
# Builds build/OpenCloud-Bar.dmg with rclone inside the app: drag the app to Applications.
set -euo pipefail
cd "$(dirname "$0")/.."
# rclone ships one build per architecture; join them so the app runs on Apple silicon and Intel.
RCLONE_DIR="$(mktemp -d)"
./scripts/fetch-rclone.sh osx arm64 "$RCLONE_DIR/arm64"
./scripts/fetch-rclone.sh osx amd64 "$RCLONE_DIR/amd64"
lipo -create "$RCLONE_DIR/arm64/rclone" "$RCLONE_DIR/amd64/rclone" -output "$RCLONE_DIR/rclone"
cp "$RCLONE_DIR/arm64/rclone-LICENSE.txt" "$RCLONE_DIR/rclone-LICENSE.txt"
RCLONE_DIR="$RCLONE_DIR" ./scripts/build-app.sh
STAGE="$(mktemp -d)"
cp -R "build/OpenCloud Bar.app" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f build/OpenCloud-Bar.dmg
# Size the image explicitly: hdiutil's own estimate is sometimes too small on CI ("No space left").
SIZE_MB=$(( $(du -sm "$STAGE" | cut -f1) + 40 ))
# hdiutil sometimes fails with "Resource busy" on CI machines; retry a few times.
for i in 1 2 3 4 5; do
  hdiutil create -volname "OpenCloud Bar" -srcfolder "$STAGE" -fs HFS+ -size "${SIZE_MB}m" -ov -format UDZO build/OpenCloud-Bar.dmg && break
  [ "$i" = 5 ] && exit 1
  sleep $((i * 5))
done
echo "built build/OpenCloud-Bar.dmg"
