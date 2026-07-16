# Backporting the Android session to the other clients

The Android bring-up (branch `android-fixes`) produced a lot of change. Most
of the value lives in the **shared engine** (`internal/`, `third_party/`),
so the web UI (`cmd/fuji-cull`), the Linux/macOS native GUI
(`cmd/fuji-cull-gui`) and their bundles inherit it automatically once
rebuilt and released. A smaller set is Android-only UI worth replicating,
and some is platform-specific and not applicable.

Compiled 2026-07-16.

---

## A. Shared engine — all clients get these on the next desktop release

These commits touched `internal/` / `third_party/` only and are already in
the code every client links:

1. **Card-wide bulk index** (`lsprops-all`): the whole card's file list via
   ~4 `GetObjectPropList` requests instead of one `GetObjectInfo` per file —
   a 19k index drops from minutes to seconds. (The X-H2S only honors the
   handle=0xFFFFFFFF / depth-0 shape; depth-scoped and format-filtered
   queries fail — `lsprops-probe` discovered this.)
2. **Handle-diff catalog refresh + persistent cache**
   (`catalog-cache.json`, v2 with dates): repeat connects index in seconds —
   one `GetObjectHandles` per folder, per-file info only for genuinely new
   files; in-camera deletions drop out. `POST /api/rescan` forces a full
   re-read.
3. **Capture dates in the catalog** (`shotDTO.date`) — the data is now
   available to every client (needs UI to display; see §B).
4. **One persistent serve-parts session for ALL partial reads**
   (heads / orientation / posters / probes) — pays session setup once
   instead of per batch. Faster thumbnail sweeps everywhere, not just phone.
5. **Scheduler heartbeat** (2 s): tripped breakers and post-error backoffs
   resume on their own instead of waiting for user interaction.
6. **Poster pipeline**: 8 MB head pulls, release the stream immediately,
   idle poster sweep, transient-vs-permanent failure handling.
7. **serve-parts deadlock fix**: `Server.Close` no longer blocks behind a
   wedged read — a real streaming bug that could hang the desktop too.
8. **Graceful engine shutdown**: drain in-flight work before closing camera
   sessions; never hard-kill mid-transfer (hard kills wedge the X-H2S).
9. **Never send a USB reset (`-R`) under the persistent parts session**;
   capture serve-parts stderr into error messages.
10. **Thumb-sweep starvation fix**: window image fills and the head sweep
    take turns so a cold buffer doesn't starve the grid.
11. **`ffmpegBin()` env override + `PostersAvailable()`**: engine poster
    path is now configurable (desktop keeps using system ffmpeg).

### ⚠ Action needed: point the bulk path at the PATCHED aft

The new bulk commands (`lsprops-all`, `ls-handles`, `info-id`, `lsprops`)
live in the patched aft and run through the **stock** aft resolver
(`FUJI_AFT` / PATH `aft-mtp-cli`), NOT the `-part` binary
(`FUJI_AFT_PART`). So:

- **AppImage** — already covered: `build-appimage.sh` symlinks
  `usr/bin/aft-mtp-cli → aft-mtp-cli-part` and `AppRun` prepends `usr/bin`
  to PATH, so `LookPath` finds the patched binary. Fast index works. ✅
- **macOS bundle** — NOT covered: it ships only `aft-mtp-cli-part`. On a Mac
  with Homebrew `android-file-transfer`, `LookPath("aft-mtp-cli")` finds the
  UNPATCHED Homebrew binary → the bulk commands error and silently fall back
  to the slow per-folder `lsext` path. **Fix:** in `build-macos.sh`, also
  place an `aft-mtp-cli` (symlink or copy of the patched binary) in the
  bundle's `MacOS/` dir, or set `FUJI_AFT` to the patched path in
  `setupBundleEnv`.
- **Headless web binary run on the desktop** — same: set `FUJI_AFT` to the
  patched binary (or install a patched `aft-mtp-cli` in PATH), otherwise the
  fast index / dates / handle-diff never engage.

Rebuild note: the vendored patch (`third_party/aft-partial/partial-reads.patch`)
gained the new commands + capture dates, so the desktop bundles must rebuild
aft from the current patch.

---

## B. New engine API the web UI / native GUI can adopt (needs wiring)

The endpoints/fields exist; the desktop clients just don't render them yet:

- **`shotDTO.date`** → build the timeline: month-band + day-group headers and
  a date scrubber (see §C). Neither the web grid nor the GUI groups by date
  today.
- **Grid density control** → adjustable images-per-row (Android pinches; web
  could use a slider or `+`/`-`, the GUI a hotkey).
- **`POST /api/rescan`** → a "rescan card" button (card swaps, in-camera
  deletions).
- **`/api/status.posters`** → informational (is engine-side poster
  extraction available).

---

## C. Android-only UI worth REPLICATING in web + native GUI

The underlying data is already there (§B); these are UX ports:

- **Immich-style timeline**: large "September 2023" month bands, "Tue, Oct 3,
  2023" day headers, and a right-edge scrubber (month labels down the rail +
  a draggable handle with a month bubble) for fast jumps.
- **Adjustable images-per-row**, persisted.

The web UI already has the grid, zoom, decisions, import, Immich badges and
filmstrip — the timeline grouping + scrubber are the missing pieces.

---

## D. Android-only — NOT applicable to the desktop clients

Different platform constraints; nothing to port:

- **Bundled minimal ffmpeg** — desktop uses system ffmpeg.
- **libmpv 4:2:2 playback** — the native GUI already embeds mpv
  (`internal/mpvgl`) with software fallback; it should already play your
  4:2:2 10-bit clips (verify — see §E).
- **USB self-heal via usbfs fd**: device reset, link-dead detection,
  connection rebuild, replug guidance — the desktop uses `/dev/bus/usb`;
  these were phone-USB-stack problems (mid-transfer kills, degraded-link
  reconnects).
- **Full-image pulls via the parts session** (`fetchItemsViaParts`) — gated
  to the fd path; the desktop's one-shot `get-id` is fine on a stable link.
- **Compose/Coil/OkHttp perf** (recomposition fixes, off-main-thread
  polling, preload debounce, 512 MB disk cache) — the web UI and SDL GUI have
  their own rendering/caching stacks.
- **In-app shareable log** — desktop has real log files / a terminal.
- **Android packaging/CI** (committed keystore, non-zipped artifact, rolling
  prerelease) and the **USB connection guidance text**.

---

## E. Known gaps

- **Web UI 4:2:2 video playback**: browsers can't decode 4:2:2 10-bit HEVC
  and can't embed libmpv. The GUI and Android cover this; the browser tab
  won't without server-side transcoding (heavy — only if it matters).
- **Verify GUI 4:2:2 playback** on the MS-01's Intel iGPU: mpv hwdec=vaapi
  may or may not do 4:2:2; if not it should fall back to software. Confirm a
  4:2:2 clip actually plays in the native GUI.

---

## Dev tooling (shared benefit, not a client feature)

- **`cmd/aft-sim` fake camera + `scripts/sim-e2e.sh`**: exercises the whole
  engine (discovery, head sweep, streaming, import, breakers) with no
  hardware. Useful for regression-testing the desktop clients too, not just
  Android.
