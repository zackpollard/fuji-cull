#!/usr/bin/env bash
# Builds the macOS release tarball: all three binaries plus aft-mtp-cli-part
# built from the vendored partial-read patch. Runtime tools (gphoto2, ffmpeg,
# exiftool) and the GUI's dylibs come from Homebrew — see the bundled README.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
WORK="$ROOT/dist/macos"
OUT="$WORK/fuji-cull"
ARCH="$(uname -m)"
BREW="$(brew --prefix)"
rm -rf "$OUT"
mkdir -p "$OUT"

export CGO_CFLAGS="-I$BREW/include"
export CGO_LDFLAGS="-L$BREW/lib"
export PKG_CONFIG_PATH="$BREW/lib/pkgconfig:${PKG_CONFIG_PATH:-}"

echo "== go binaries"
go build -trimpath -ldflags "-s -w" -o "$OUT/fuji-cull-gui" "$ROOT/cmd/fuji-cull-gui"
go build -trimpath -ldflags "-s -w" -o "$OUT/fuji-cull" "$ROOT/cmd/fuji-cull"
go build -trimpath -ldflags "-s -w" -o "$OUT/fuji-import" "$ROOT/cmd/fuji-import"

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
cp "$AFT/build/cli/aft-mtp-cli" "$OUT/aft-mtp-cli-part"
ln -sf aft-mtp-cli-part "$OUT/aft-mtp-cli"

cat > "$OUT/README.md" <<EOF
# fuji-cull (macOS $ARCH)

Runtime dependencies come from Homebrew:

    brew install sdl2 sdl2_ttf jpeg-turbo mpv libgphoto2 gphoto2 ffmpeg exiftool

- \`fuji-cull-gui\` — native culling UI (links the brew dylibs above)
- \`fuji-cull\` — web UI server (browse from any device)
- \`fuji-import\` — headless bulk importer
- \`aft-mtp-cli-part\` — patched MTP client (partial reads: thumbnails,
  streaming); found automatically when kept next to the app binaries

Keep the files in one directory and run from there, e.g.:

    ./fuji-cull-gui --listen 127.0.0.1:8787
EOF

TAR="$WORK/fuji-cull-macos-$ARCH.tar.gz"
tar -czf "$TAR" -C "$WORK" fuji-cull
echo "== done: $TAR"
