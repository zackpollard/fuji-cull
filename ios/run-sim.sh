#!/usr/bin/env bash
# Build the iOS app and run it in the simulator against the fake backend.
#   ./run-sim.sh            build + install + launch
#   ./run-sim.sh --bind     also re-run `gomobile bind` (after engine changes)
#   ./run-sim.sh --shot P    ... then screenshot to path P after a short wait
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOFLAGS=-mod=mod

DEV="${SIM_DEVICE:-iPad Pro 11-inch (M4)}"
BUNDLE=pro.zackpollard.fujicull
SHOT=""
BIND=0
CLEAN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --bind) BIND=1 ;;
    --clean) CLEAN=1 ;;
    --shot) SHOT="$2"; shift ;;
  esac
  shift
done

if [ "$BIND" = 1 ]; then
  echo "== gomobile bind (engine → Mobile.xcframework) =="
  gomobile bind -target=ios -o Mobile.xcframework ../mobile
fi

echo "== xcodegen =="
xcodegen generate --quiet
# Xcode 15.4 cannot read xcodegen's default objectVersion 77.
sed -i '' -e 's/objectVersion = 77;/objectVersion = 56;/' \
          -e '/preferredProjectObjectVersion = 77;/d' FujiCull.xcodeproj/project.pbxproj

echo "== build =="
set -o pipefail
xcodebuild -project FujiCull.xcodeproj -scheme FujiCull \
  -destination "platform=iOS Simulator,name=$DEV" \
  -derivedDataPath /tmp/fc-dd CODE_SIGNING_ALLOWED=NO build \
  2>&1 | grep -iE "error:|BUILD SUCCEEDED|BUILD FAILED" || { echo "build failed"; exit 1; }

APP=/tmp/fc-dd/Build/Products/Debug-iphonesimulator/FujiCull.app
DEVID=$(xcrun simctl list devices | grep "$DEV (" | grep -oE '[0-9A-Fa-f-]{36}' | head -1)
xcrun simctl boot "$DEVID" 2>/dev/null || true
if [ "$CLEAN" = 1 ]; then
  xcrun simctl uninstall "$DEVID" "$BUNDLE" 2>/dev/null || true  # wipe Documents (corpus+cache)
fi
xcrun simctl install "$DEVID" "$APP"
xcrun simctl terminate "$DEVID" "$BUNDLE" 2>/dev/null || true
xcrun simctl launch "$DEVID" "$BUNDLE" >/dev/null
echo "launched $BUNDLE on $DEV ($DEVID)"

if [ -n "$SHOT" ]; then
  sleep 8
  xcrun simctl io "$DEVID" screenshot "$SHOT" >/dev/null 2>&1
  echo "screenshot: $SHOT"
fi
