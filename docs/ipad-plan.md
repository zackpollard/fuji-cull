# iPad app plan — camera-tethered culling via ImageCaptureCore ("Path B")

Status: **planned, not started** (scoped 2026-07-15). Prerequisite reading:
the Android bring-up in git history — the iPad app reuses its architecture
wholesale and differs only in the camera transport and app shell.

## Goal

The Android workflow on an iPad: USB-C cable from the X-H2S (USB card-reader
mode) to the iPad, photos stay on camera during review, sliding NVMe-style
buffer on device, cull in a grid/viewer, import keepers to local storage +
Immich. No SD-card removal (that is "Path A", explicitly out of scope here).

## Why this is feasible

- iOS has **no exec() and no usbfs** — the patched `aft-mtp-cli` cannot run.
- Apple's **ImageCaptureCore** (ICC) is the sanctioned PTP/MTP stack and
  covers everything the engine needs:
  - device discovery + automatic content enumeration (`ICCameraDevice`,
    `ICDeviceBrowser`),
  - **partial reads**: `ICCameraFile.requestReadDataAtOffset(length:)` —
    powers the head sweep, orientation, posters, video streaming, videohead
    (iPadOS ≥14; a 4 MB read cap existed before 14, and 13.4 shipped a
    read-returns-empty regression — set the deployment floor accordingly,
    suggest iPadOS 16+),
  - full downloads: `requestDownloadOfItem`,
  - **raw PTP passthrough**: `requestSendPTPCommand` — lets us issue
    `GetObjectPropList` / `GetPartialObject` ourselves if ICC's own
    enumeration or reads prove slow, i.e. everything aft does today.
- The Go engine (catalog, prefetcher, breakers, sessions, import, Immich,
  loopback HTTP, web UI assets) compiles for iOS via `gomobile bind`
  (xcframework), same facade pattern as `mobile/` for Android.

## Architecture

```
┌────────────────────────── iPad app ──────────────────────────┐
│ SwiftUI shell (connect screen, settings, log)                │
│   └─ WKWebView → http://127.0.0.1:<port>  (existing web UI)  │
│ Swift ICCTransport (ImageCaptureCore)                        │
│   ▲ implements Go interface via gomobile reverse binding     │
│ Go engine (gomobile xcframework)                             │
│   iccBackend → Transport   prefetcher/breakers unchanged     │
│   posters: cgo libav shim (no exec on iOS)                   │
└──────────────────────────────────────────────────────────────┘
```

### 1. Transport seam (the core work)

New Go interface in `internal/cull` (or a small `internal/camtransport`
package), implemented in Swift and injected through the mobile facade:

```go
// Transport is a camera link the engine drives without exec'ing anything.
type Transport interface {
    // Folders lists NNN_FUJI folder names per camera root.
    Folders() (jsonBytes []byte, err error)
    // Entries lists one folder: objectID, name, size per file.
    Entries(folder string) (jsonBytes []byte, err error)
    // ReadAt reads size bytes at offset of an object (partial read).
    ReadAt(objectID string, offset, size int64) ([]byte, error)
    // Download pulls a whole object to destPath (progress via file size).
    Download(objectID, destPath string) error
    // Connected reports whether a camera is currently attached.
    Connected() bool
}
```

(gomobile constraint: only slices of bytes, strings, ints etc. cross the
boundary — hence JSON blobs for listings.)

- `iccBackend` implements the existing `Backend` interface on top of
  `Transport` (mirrors `cliBackend`; the catalog cache from
  `catalog-cache.json` is reused unchanged — key by folder, only re-read the
  highest-numbered folder).
- The prefetcher's partial-read path calls `Transport.ReadAt` directly in
  place of `partsReadAt`'s serve-parts session (a trivial adapter; the
  persistent-session vs one-shot claim juggling **disappears** — there are
  no processes and ICC owns the single session).
- Full pulls: chunked `ReadAt` (reuse the `fetchItemsViaParts` pattern —
  progressive writes keep incremental promotion working) or `Download`;
  benchmark both, chunked preferred for preemptability.
- All content validation (magic bytes) and both breakers stay exactly as-is
  — they are transport-agnostic and the X-H2S stale-buffer bug may well
  exist under Apple's stack too (macOS ptpcamerad experience says treat the
  camera as hostile regardless of transport).

### 2. Swift `ICCTransport`

- `ICDeviceBrowser` for attach/detach; request contents on attach.
- Enumeration: ICC populates `ICCameraDevice.contents` itself. **Unknown:
  cost on a 19k-file card** (it may run its own per-object info storm).
  Measure first; if slow, switch Entries() to raw
  `requestSendPTPCommand(GetObjectPropList)` reusing the exact dataset
  parsing the aft patch does (2 bulk round-trips per folder).
- Serialize everything on one dispatch queue: the engine already assumes a
  single-threaded MTP link.
- Map ICC's async callbacks to synchronous Transport calls with semaphores +
  timeouts (the engine has its own watchdogs; keep transport timeouts just
  above them).
- Surface attach/detach + errors into the engine log (same
  `app:`-prefixed events as Android).

### 3. App shell (phase 1 = thin)

- SwiftUI: connect screen (status + usb diag + log tail), settings screen
  (Immich URL/key, session, stack, full-rescan), log screen with share
  sheet — direct ports of the Android screens.
- Main UI: **WKWebView at the loopback port** — the existing web UI is
  feature-complete (grid, viewer, zoom, decisions, import, badges) and was
  the iPad-over-Tailscale interface already. Native SwiftUI grid/viewer is
  an optional phase 3, decided after field use.
- Keep-alive: background execution on iOS is restrictive; while the app is
  foreground this is a non-issue. Import continuation in background =
  `beginBackgroundTask` (~30 s) + user guidance to keep the app open, or
  `BGProcessingTask` later. Phase 1: imports run foreground-only.

### 4. Video

- Playback in the web UI uses AVFoundation via `<video>` — fine for
  whatever the iPad hardware decodes. **HEVC 4:2:2 10-bit hardware decode
  on iPad media engines is undocumented**; test on Zack's iPad early.
- If unsupported: **MPVKit** (libmpv xcframework, actively maintained) in a
  native video screen — identical solution to Android's MpvPlayer, with
  `hwdec=videotoolbox` where it works and software fallback where it
  doesn't. Disk-backed demuxer cache options carry over verbatim.

### 5. Posters (no exec → link ffmpeg)

- Reuse the minimal-ffmpeg recipe (n7.1, hevc/h264 + mov + mjpeg/image2)
  but build **static libraries** for ios-arm64 and a ~50-line C shim
  `extract_poster(in_path, out_jpg_path)` (avformat open → first video
  packet(s) → decode frame 0 → mjpeg encode, full-res — remember the
  minimal build's swscale RESIZE path corrupts; never scale in the shim).
- Call via cgo inside the gomobile framework, gated by `GOOS=ios` build
  tags; desktop/Android keep the exec path. The engine's poster sweep then
  works unchanged (heads via Transport.ReadAt → shim → ThumbPath).

### 6. Simulator & testing

- The aft-sim philosophy moves up one seam: a **Go fake Transport** backed
  by a local media tree (share corpus + sickness knobs with aft-sim via a
  tiny common package, or just re-implement — it is ~100 lines against the
  interface). Engine E2E then runs on macOS and in the iOS simulator with
  no camera and no exec.
- CI: macos-14 runner — gomobile bind (ios), xcodebuild the app, run the
  simulator boot smoke + loopback /api assertions (port forward not needed;
  simulator shares host network), screenshot via `xcrun simctl io
  screenshot`.
- Real-camera validation checklist (needs Zack + camera):
  1. attach/detach/reattach cycles, 2. enumeration wall-time on the 19k
  card (ICC contents vs raw GetObjectPropList), 3. partial-read correctness
  (magic validation over a few hundred heads), 4. sustained bulk pulls
  (import-scale) without bus drops, 5. stale-buffer behavior + breaker
  recovery, 6. 4:2:2 playback via AVFoundation, then MPVKit.

### 7. Distribution

- Requires an Apple Developer account ($99/yr) for practical iteration:
  **TestFlight internal builds are the android-dev-rolling equivalent**
  (push → CI → TestFlight → install notification on the iPad).
- CI signing via App Store Connect API key in repo secrets;
  `xcodebuild -exportArchive` with automatic signing.
- Free-account sideload (7-day expiry, Xcode tether) works for first
  bring-up if the paid account lags.

## Phases & rough sizing

| Phase | Scope | Size |
|---|---|---|
| 0 | Xcode/gomobile skeleton: engine boots in simulator, WKWebView shows web UI against fake Transport | 1–2 days |
| 1 | Swift ICCTransport (enumeration, ReadAt, Download) + iccBackend + connect/settings/log screens | 3–5 days |
| 2 | Real-camera validation + perf tuning (raw PTP fallbacks where ICC is slow), posters via cgo shim | 2–4 days + camera access |
| 3 (opt) | MPVKit video screen if AVFoundation rejects 4:2:2 | 1–2 days |
| 4 (opt) | Native SwiftUI grid/viewer replacing WKWebView | 1–2 weeks, only if the web UI feels wrong on-device |

## Key risks

| Risk | Mitigation |
|---|---|
| ICC enumeration slow/heavy on 19k files | raw `GetObjectPropList` via `requestSendPTPCommand`; catalog cache limits exposure to one folder per attach |
| ICC partial-read regressions (13.4 history) | deployment target iPadOS 16+; magic-validate everything (already engine policy); chunked Download fallback |
| X-H2S stale-buffer bug under Apple's stack | breakers + validation are transport-agnostic and stay; power-cycle guidance UX carries over |
| 4:2:2 playback unsupported by hardware | MPVKit software decode (proven approach on Android) |
| Apple review (if ever App Store) | personal TestFlight/ad-hoc only; no review needed |

## Open questions for Zack

1. Which iPad exactly (M-series? iPadOS version?) — decides the 4:2:2
   hardware-decode test and the deployment floor.
2. Apple Developer account: existing/paid? (Gates TestFlight-style
   iteration.)
3. Web-UI-in-WKWebView acceptable as the phase-1/2 UI, with native SwiftUI
   as a later call?
