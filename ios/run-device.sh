#!/usr/bin/env bash
# Build fuji-cull and install it on a tethered iPad, signed with a FREE Apple ID
# (Xcode "Personal Team") — no paid Apple Developer Program needed.
#
#   ./run-device.sh            build + install to the connected device
#   ./run-device.sh --bind     also re-run `gomobile bind` first
#   ./run-device.sh --list     list connected devices
#
# Free-account caveats (see README): the signature expires after 7 DAYS — just
# re-run this script to refresh it. Max 3 sideloaded apps per device.
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOFLAGS=-mod=mod

if [ "${1:-}" = "--list" ]; then
  xcrun devicectl list devices
  exit 0
fi

TEAM="${FUJI_TEAM_ID:-$(cat .teamid 2>/dev/null || true)}"
if [ -z "$TEAM" ]; then
  cat <<'EOF'
No Apple Team ID configured.

1. Xcode ▸ Settings ▸ Accounts ▸ "+" ▸ add your (free) Apple ID.
2. Find the personal team id:
     xcrun security find-identity -v -p codesigning
   or open ios/FujiCull.xcodeproj ▸ target ▸ Signing & Capabilities and read
   the Team dropdown.
3. Save it (gitignored):
     echo YOURTEAMID > ios/.teamid
EOF
  exit 1
fi

if [ "${1:-}" = "--bind" ]; then
  echo "== gomobile bind =="
  gomobile bind -target=ios -o Mobile.xcframework ../mobile
fi

echo "== xcodegen =="
xcodegen generate --quiet
# Xcode <16 can't read xcodegen's default project format (objectVersion 77).
XCODE_MAJOR=$(xcodebuild -version 2>/dev/null | head -1 | sed -E 's/Xcode ([0-9]+).*/\1/')
if [ "${XCODE_MAJOR:-0}" -lt 16 ]; then
  sed -i '' -e 's/objectVersion = 77;/objectVersion = 56;/' \
            -e '/preferredProjectObjectVersion = 77;/d' FujiCull.xcodeproj/project.pbxproj
fi

# Connected device: xcodebuild wants the hardware UDID, devicectl its own id.
xcrun devicectl list devices --json-output /tmp/fc-devices.json >/dev/null 2>&1 || true
read -r UDID COREID <<EOF
$(python3 - <<'PY'
import json
try:
    d = json.load(open('/tmp/fc-devices.json'))
except Exception:
    raise SystemExit()
for dev in d.get('result', {}).get('devices', []):
    if dev.get('connectionProperties', {}).get('tunnelState') in ('connected', 'available', 'connecting'):
        print(dev.get('hardwareProperties', {}).get('udid', ''), dev.get('identifier', ''))
        break
PY
)
EOF
if [ -z "${UDID:-}" ]; then
  echo "No connected iPad found. Plug it in (or pair over Wi-Fi), unlock it, trust this Mac."
  echo "(list devices with: ./run-device.sh --list)"
  exit 1
fi
echo "== device: $UDID =="

echo "== build (signed, team $TEAM) =="
# Target the device by UDID: that is what makes Xcode register it with your
# Apple ID and mint a provisioning profile that includes it.
xcodebuild -project FujiCull.xcodeproj -scheme FujiCull \
  -destination "platform=iOS,id=$UDID" \
  -derivedDataPath /tmp/fc-dd-device \
  DEVELOPMENT_TEAM="$TEAM" CODE_SIGN_STYLE=Automatic \
  -allowProvisioningUpdates \
  build 2>&1 | grep -iE "error:|BUILD SUCCEEDED|BUILD FAILED" || { echo "build failed"; exit 1; }
DEVID="$COREID"

APP=/tmp/fc-dd-device/Build/Products/Debug-iphoneos/FujiCull.app
echo "== install =="
xcrun devicectl device install app --device "$DEVID" "$APP"

cat <<EOF

Installed. On first launch the iPad will refuse to run it until you trust the
certificate:  Settings ▸ General ▸ VPN & Device Management ▸ (your Apple ID) ▸ Trust.

Free-account signing expires after 7 days — re-run this script to refresh.
EOF
