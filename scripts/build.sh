#!/bin/bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
mkdir -p bin
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
if [[ "$(uname -s)" != Darwin ]]; then
  go build -trimpath -ldflags='-s -w' -o bin/macbridge ./cmd/macbridge
  exit
fi
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "$scratch/macbridge-$arch" ./cmd/macbridge
done
lipo -create "$scratch/macbridge-arm64" "$scratch/macbridge-amd64" -output bin/macbridge
app="$root/MacBridge Go.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources/chrome-extension"
for arch in arm64 x86_64; do
  xcrun swiftc -swift-version 5 -parse-as-library -O -target "$arch-apple-macos13.0" menubar/MacBridge.swift -framework AppKit -o "$scratch/menu-$arch"
done
lipo -create "$scratch/menu-arm64" "$scratch/menu-x86_64" -output "$app/Contents/MacOS/MacBridge"
cp bin/macbridge "$app/Contents/MacOS/macbridge-core"
cp chrome-extension/*.js chrome-extension/manifest.json chrome-extension/workspace.html "$app/Contents/Resources/chrome-extension/"
rm -f "$app/Contents/Resources/chrome-extension/"*.test.js
cp menubar/Info.plist "$app/Contents/Info.plist"
codesign --force --sign - bin/macbridge
codesign --force --sign - "$app/Contents/MacOS/macbridge-core"
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
# Case-insensitive macOS volumes must retain both the AppKit menu and Go core.
test -x "$app/Contents/MacOS/MacBridge"
test -x "$app/Contents/MacOS/macbridge-core"
test ! "$app/Contents/MacOS/MacBridge" -ef "$app/Contents/MacOS/macbridge-core"
otool -L "$app/Contents/MacOS/MacBridge" | grep -q '/AppKit.framework/'
"$app/Contents/MacOS/macbridge-core" version
printf 'Built bin/macbridge and MacBridge Go.app (Apple Silicon + Intel).\n'
