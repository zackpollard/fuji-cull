#!/usr/bin/env bash
# Builds a self-contained fuji-cull-gui AppImage: the cgo library closure
# (sdl2, sdl2_ttf, turbojpeg, mpv) plus every external tool the app execs at
# runtime — gphoto2 (with its camlib/iolib plugins), a static ffmpeg,
# exiftool, and aft-mtp-cli-part built from the vendored partial-read patch.
#
# Designed for ubuntu CI (apt package layout); run from the repo root.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
WORK="$ROOT/dist/appimage"
APPDIR="$WORK/AppDir"
ARCH="$(uname -m)"
rm -rf "$APPDIR"
mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/lib" "$APPDIR/usr/share/perl5"

echo "== fuji-cull-gui"
go build -trimpath -ldflags "-s -w" -o "$APPDIR/usr/bin/fuji-cull-gui" "$ROOT/cmd/fuji-cull-gui"

echo "== patched aft-mtp-cli (partial reads)"
AFT="$WORK/aft"
if [ ! -x "$AFT/build/cli/aft-mtp-cli" ]; then
  rm -rf "$AFT"
  git clone https://github.com/whoozle/android-file-transfer-linux.git "$AFT"
  git -C "$AFT" checkout "$(cat "$ROOT/third_party/aft-partial/UPSTREAM_COMMIT")"
  git -C "$AFT" apply "$ROOT/third_party/aft-partial/partial-reads.patch"
  cmake -S "$AFT" -B "$AFT/build" -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_QT_UI=OFF -DBUILD_FUSE=OFF -DBUILD_SHARED_LIB=OFF
  cmake --build "$AFT/build" -j"$(nproc)" --target aft-mtp-cli
fi
cp "$AFT/build/cli/aft-mtp-cli" "$APPDIR/usr/bin/aft-mtp-cli-part"
# the patched binary is a superset of stock; serve both roles with one file
ln -sf aft-mtp-cli-part "$APPDIR/usr/bin/aft-mtp-cli"

echo "== gphoto2 + libgphoto2 plugins"
cp "$(command -v gphoto2)" "$APPDIR/usr/bin/"
CAMLIB_DIR="$(pkg-config --variable=driverdir libgphoto2)"
IOLIB_DIR="$(pkg-config --variable=driverdir libgphoto2_port)"
cp -r "$CAMLIB_DIR" "$APPDIR/usr/lib/libgphoto2"
cp -r "$IOLIB_DIR" "$APPDIR/usr/lib/libgphoto2_port"

echo "== exiftool (perl script + module tree)"
cp "$(command -v exiftool)" "$APPDIR/usr/bin/"
cp -r /usr/share/perl5/Image "$APPDIR/usr/share/perl5/"
cp -r /usr/share/perl5/File "$APPDIR/usr/share/perl5/" 2>/dev/null || true

echo "== static ffmpeg"
if [ ! -x "$WORK/ffmpeg-static/ffmpeg" ]; then
  mkdir -p "$WORK/ffmpeg-static"
  curl -fsSL "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz" |
    tar xJ -C "$WORK/ffmpeg-static" --strip-components=1
fi
cp "$WORK/ffmpeg-static/ffmpeg" "$APPDIR/usr/bin/"

echo "== linuxdeploy (library closure)"
LD="$WORK/linuxdeploy"
if [ ! -x "$LD" ]; then
  curl -fsSL -o "$LD" \
    "https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-$ARCH.AppImage"
  chmod +x "$LD"
fi
# collect shared-library closures for the dynamic executables and plugins;
# ffmpeg is static and exiftool is a script, so they need no closure
export APPIMAGE_EXTRACT_AND_RUN=1
export LDAI_OUTPUT="$WORK/fuji-cull-gui-$ARCH.AppImage"
"$LD" --appdir "$APPDIR" \
  --executable "$APPDIR/usr/bin/fuji-cull-gui" \
  --executable "$APPDIR/usr/bin/aft-mtp-cli-part" \
  --executable "$APPDIR/usr/bin/gphoto2" \
  --deploy-deps-only "$APPDIR/usr/lib/libgphoto2" \
  --deploy-deps-only "$APPDIR/usr/lib/libgphoto2_port" \
  --desktop-file "$ROOT/assets/fuji-cull.desktop" \
  --icon-file "$ROOT/assets/fuji-cull.png" \
  --custom-apprun "$ROOT/scripts/appimage/AppRun" \
  --output appimage

echo "== done: $LDAI_OUTPUT"
