#!/usr/bin/env bash
# Builds fuji-cull.app (drag-to-Applications dmg): the GUI plus every helper
# tool it execs (gphoto2 + plugins, ffmpeg, exiftool, patched aft-mtp-cli)
# with the full dylib closure bundled and install names rewritten, ad-hoc
# signed. Unsigned-by-Apple: first launch needs right-click -> Open.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
WORK="$ROOT/dist/macos"
APP="$WORK/fuji-cull.app"
ARCH="$(uname -m)"
BREW="$(brew --prefix)"
MACOS="$APP/Contents/MacOS"
RES="$APP/Contents/Resources"
LIBS="$APP/Contents/libs"
rm -rf "$APP"
mkdir -p "$MACOS" "$RES/perl5" "$LIBS"

export CGO_CFLAGS="-I$BREW/include"
export CGO_LDFLAGS="-L$BREW/lib"
export PKG_CONFIG_PATH="$BREW/lib/pkgconfig:${PKG_CONFIG_PATH:-}"

echo "== go binaries"
go build -trimpath -ldflags "-s -w" -o "$MACOS/fuji-cull-gui" "$ROOT/cmd/fuji-cull-gui"
go build -trimpath -ldflags "-s -w" -o "$MACOS/fuji-cull" "$ROOT/cmd/fuji-cull"
go build -trimpath -ldflags "-s -w" -o "$MACOS/fuji-import" "$ROOT/cmd/fuji-import"

echo "== patched aft-mtp-cli (partial reads)"
AFT="$WORK/aft"
if [ ! -x "$AFT/build/cli/aft-mtp-cli" ]; then
  rm -rf "$AFT"
  git clone https://github.com/whoozle/android-file-transfer-linux.git "$AFT"
  git -C "$AFT" checkout "$(cat "$ROOT/third_party/aft-partial/UPSTREAM_COMMIT")"
  git -C "$AFT" apply "$ROOT/third_party/aft-partial/partial-reads.patch"
  cmake -S "$AFT" -B "$AFT/build" -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_QT_UI=OFF -DBUILD_FUSE=OFF -DBUILD_SHARED_LIB=OFF
  cmake --build "$AFT/build" -j"$(sysctl -n hw.ncpu)" --target aft-mtp-cli
fi
cp "$AFT/build/cli/aft-mtp-cli" "$MACOS/aft-mtp-cli-part"
cp "$AFT/build/cli/aft-mtp-cli" "$MACOS/aft-mtp-cli"

echo "== helper tools"
cp "$BREW/bin/gphoto2" "$MACOS/"
cp "$BREW/bin/ffmpeg" "$MACOS/"
# exiftool is a perl script: bundle signing rejects non-Mach-O in MacOS/,
# so scripts live under Resources/bin (on PATH via setupBundleEnv)
mkdir -p "$RES/bin"
cp "$(readlink -f "$BREW/bin/exiftool")" "$RES/bin/exiftool"
# exiftool's perl module tree: locate Image/ExifTool.pm under the formula
# (brew layouts vary between lib and libexec across versions)
EXIF_ROOT="$(dirname "$(dirname "$(readlink -f "$BREW/bin/exiftool")")")"
PM="$(find "$EXIF_ROOT" -name ExifTool.pm -path "*Image*" | head -1)"
cp -R "$(dirname "$(dirname "$PM")")/." "$RES/perl5/"
# libgphoto2 plugin trees (dlopened at runtime via CAMLIBS/IOLIBS)
cp -R "$(pkg-config --variable=driverdir libgphoto2)" "$RES/libgphoto2"
cp -R "$(pkg-config --variable=driverdir libgphoto2_port)" "$RES/libgphoto2_port"

echo "== dylib closure"
brew list dylibbundler >/dev/null 2>&1 || brew install dylibbundler
fixup() {
  dylibbundler -of -b -x "$1" -d "$LIBS" -p @executable_path/../libs >/dev/null
}
fixup "$MACOS/fuji-cull-gui"
fixup "$MACOS/aft-mtp-cli-part"
fixup "$MACOS/aft-mtp-cli"
fixup "$MACOS/gphoto2"
fixup "$MACOS/ffmpeg"
for plugin in "$RES/libgphoto2"/*.so "$RES/libgphoto2_port"/*.so; do
  [ -f "$plugin" ] && fixup "$plugin"
done

echo "== sdl3"
# Homebrew's sdl2 is sdl2-compat, which dlopens the real SDL3 at RUNTIME —
# invisible to dylibbundler's link-time walk, so without this the app dies
# with "Failed loading SDL3 library" on any Mac without Homebrew. dlopen of
# a bare library name searches the caller's/main executable's rpaths, so
# bundle libSDL3 and point an rpath at the libs dir.
cp -P "$BREW/lib/libSDL3"*.dylib "$LIBS/"
install_name_tool -add_rpath "@executable_path/../libs" "$MACOS/fuji-cull-gui"
test -e "$LIBS/libSDL3.0.dylib" || { echo "libSDL3 missing from bundle"; exit 1; }
otool -l "$MACOS/fuji-cull-gui" | grep -q "@executable_path/../libs" || { echo "rpath missing"; exit 1; }

echo "== bundle metadata"
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>fuji-cull</string>
  <key>CFBundleDisplayName</key><string>fuji-cull</string>
  <key>CFBundleIdentifier</key><string>pro.zackpollard.fuji-cull</string>
  <key>CFBundleVersion</key><string>${VERSION:-0.0.0}</string>
  <key>CFBundleShortVersionString</key><string>${VERSION:-0.0.0}</string>
  <key>CFBundleExecutable</key><string>fuji-cull-gui</string>
  <key>CFBundleIconFile</key><string>fuji-cull</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
</dict></plist>
EOF

echo "== icon"
ICONSET="$WORK/fuji-cull.iconset"
rm -rf "$ICONSET"; mkdir -p "$ICONSET"
for sz in 16 32 128 256 512; do
  sips -z $sz $sz "$ROOT/assets/fuji-cull.png" --out "$ICONSET/icon_${sz}x${sz}.png" >/dev/null
  sips -z $((sz*2)) $((sz*2)) "$ROOT/assets/fuji-cull.png" --out "$ICONSET/icon_${sz}x${sz}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$RES/fuji-cull.icns"

echo "== ad-hoc sign"
find "$LIBS" -name "*.dylib" -exec codesign --force -s - {} \;
find "$RES/libgphoto2" "$RES/libgphoto2_port" -name "*.so" -exec codesign --force -s - {} \; 2>/dev/null || true
for bin in "$MACOS"/*; do
  [ -f "$bin" ] && file "$bin" | grep -q Mach-O && codesign --force -s - "$bin"
done
codesign --force -s - "$APP"

echo "== dmg"
DMG="$WORK/fuji-cull-macos-$ARCH.dmg"
rm -f "$DMG"
hdiutil create -volname fuji-cull -srcfolder "$APP" -ov -format UDZO "$DMG" >/dev/null
echo "== done: $DMG"
