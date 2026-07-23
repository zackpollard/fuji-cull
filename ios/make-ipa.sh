#!/usr/bin/env bash
# Build an UNSIGNED release .ipa for sideload distribution (AltStore /
# SideStore / Sideloadly re-sign it per user with their own free Apple ID —
# the only iOS distribution route that needs no paid developer account).
#
#   ./make-ipa.sh            build dist/FujiCull.ipa
#   ./make-ipa.sh --bind     also re-run `gomobile bind` first
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOFLAGS=-mod=mod

if [ "${1:-}" = "--bind" ]; then
  echo "== gomobile bind =="
  gomobile bind -target=ios -o Mobile.xcframework ../mobile
fi

echo "== xcodegen =="
xcodegen generate --quiet || { sleep 2; xcodegen generate --quiet; }

# Xcode <16 can't read xcodegen's default project format (objectVersion 77).
XCODE_MAJOR=$(xcodebuild -version 2>/dev/null | head -1 | sed -E 's/Xcode ([0-9]+).*/\1/')
if [ "${XCODE_MAJOR:-0}" -lt 16 ]; then
  sed -i '' -e 's/objectVersion = 77;/objectVersion = 56;/' \
            -e '/preferredProjectObjectVersion = 77;/d' FujiCull.xcodeproj/project.pbxproj
fi

echo "== release build (unsigned, version ${VERSION:-0.1.0}) =="
xcodebuild -project FujiCull.xcodeproj -scheme FujiCull \
  -configuration Release \
  -destination "generic/platform=iOS" \
  -derivedDataPath /tmp/fc-dd-release \
  CODE_SIGNING_ALLOWED=NO \
  MARKETING_VERSION="${VERSION:-0.1.0}" \
  build 2>&1 | grep -E "error:|BUILD SUCCEEDED|BUILD FAILED" || { echo "build failed"; exit 1; }

APP=/tmp/fc-dd-release/Build/Products/Release-iphoneos/FujiCull.app
[ -d "$APP" ] || { echo "no .app at $APP"; exit 1; }

echo "== package =="
STAGE=$(mktemp -d)
mkdir -p "$STAGE/Payload" ../dist
cp -R "$APP" "$STAGE/Payload/"
(cd "$STAGE" && zip -qry ipa.zip Payload)
mv "$STAGE/ipa.zip" ../dist/FujiCull.ipa
rm -rf "$STAGE"
du -h ../dist/FujiCull.ipa | cut -f1 | xargs echo "dist/FujiCull.ipa:"
