#!/bin/bash
# Copies the built app to ~/Applications and starts it.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -d "build/OpenCloud Bar.app" ] || ./scripts/build-app.sh
mkdir -p "$HOME/Applications"
pkill -x OpenCloudBar 2>/dev/null && sleep 2 || true
rm -rf "$HOME/Applications/OpenCloud Bar.app"
cp -R "build/OpenCloud Bar.app" "$HOME/Applications/"
open "$HOME/Applications/OpenCloud Bar.app"
echo "installed ~/Applications/OpenCloud Bar.app (turn on Open at Login from its menu)"
