# fuji-cull — iPad app

Native SwiftUI client of the fuji-cull engine (the same Go core the desktop and
Android apps run), driving it over a loopback HTTP API. Architecture:
`docs/ipad-plan.md`.

## Camera link

| build | link | how |
|---|---|---|
| device | `StartICC` | `ICCTransport` — Apple's **ImageCaptureCore** (iOS has no exec/usbfs, so the patched `aft-mtp-cli` can't run) |
| simulator / no camera | `StartLocal` | `dir` backend over a synthetic corpus (`SeedFakeCorpus`) |

The app prefers the camera: it waits ~5 s for ImageCaptureCore to report an
attached device, else falls back to the fake corpus. Plug a camera in later and
it restarts onto the camera automatically. *Force fake corpus* in Settings
pins it to the fake backend (handy for UI work on-device).

Partial reads (head-sweep thumbnails, EXIF orientation, chunked image pulls) all
ride `Transport.ReadAt`; the stale-buffer breakers and magic-byte validation are
unchanged, because that X-H2S firmware bug is transport-agnostic.

## Build & run — simulator

Requires `go`, `gomobile`, `xcodegen`, Xcode.

```sh
./run-sim.sh --bind          # gomobile bind engine → Mobile.xcframework, then build+run
./run-sim.sh                 # rebuild+run (engine unchanged)
./run-sim.sh --clean         # also wipe app Documents (re-seed the fake corpus)
./run-sim.sh --shot out.png  # screenshot after launch
```

## Build & run — iPad (free Apple ID, no paid account)

We deliberately avoid the $99/yr Apple Developer Program. The supported route is
**free-Apple-ID sideloading** via Xcode's "Personal Team":

1. Xcode ▸ Settings ▸ Accounts ▸ **+** ▸ sign in with any Apple ID (free).
2. Save the team id (gitignored): `echo YOURTEAMID > ios/.teamid`
3. Plug in the iPad, unlock it, trust the Mac, then:

```sh
./run-device.sh --bind    # build, sign, install
./run-device.sh --list    # show connected devices
```

4. First launch: iPad ▸ Settings ▸ General ▸ VPN & Device Management ▸ your
   Apple ID ▸ **Trust**.

### Free-account limits (and how we live with them)

- **7-day signature expiry** — the app stops launching after a week. Re-run
  `./run-device.sh` to refresh (takes seconds; the culling session, buffer and
  settings all survive because they live in the app's Documents).
- **3 sideloaded apps per device**, 10 app IDs/week.
- **No TestFlight** (paid-only) and no App Store — irrelevant here, this is a
  personal tool.
- Optional convenience: **AltStore / SideStore** can re-sign and refresh the
  same free-account build over Wi-Fi on a schedule, so you don't need the cable
  every 7 days. It uses the identical certificate; nothing in the app changes.

If a paid account ever appears, the only delta is TestFlight distribution — the
project already signs with automatic provisioning.

## Layout

- `project.yml` — xcodegen spec (deployment target iOS 16; `run-sim.sh` /
  `run-device.sh` downgrade the generated project to Xcode 15.4's format).
- `FujiCull/`
  - `FujiCullApp.swift` — app shell, rebuilds on engine epoch
  - `Engine.swift` — gomobile engine wrapper + camera/fake mode selection
  - `ICCTransport.swift` — ImageCaptureCore implementation of the Go `Transport`
  - `API.swift` — loopback HTTP client (same endpoints as Android/web)
  - `ConnectView` / `GridView` / `ViewerView` / `ImportView` / `SettingsView`
  - `Keyboard.swift` — hardware-keyboard culling (K/X/C, arrows, Esc)
- engine facade: `../mobile/ios.go` (`StartLocal`, `StartICC`, `SeedFakeCorpus`).

`Mobile.xcframework` (gomobile) and `FujiCull.xcodeproj` (xcodegen) are
generated, not committed — the run scripts produce both.

## Feature parity with the Android app

Grid (Immich-style month/day timeline, month scrubber, decision bars, Immich
badges, buffered indicators, video badges, `CAMERA SICK`, th/ex counters),
viewer (paged, pinch-zoom, filmstrip, keep/reject/clear with auto-advance),
video playback, import dialog with live progress, settings (Immich URL/key,
session, RAF+JPG stacking, full rescan), diagnostics log with share.

## Still to validate on real hardware

1. ImageCaptureCore enumeration wall-time on a ~19k-file card (if slow, the plan's
   fallback is raw `requestSendPTPCommand(GetObjectPropList)`).
2. Partial-read correctness over a few hundred heads (magic-byte validated).
3. Sustained bulk pulls at import scale without bus drops.
4. Stale-buffer behaviour + breaker recovery under Apple's stack.
5. 4:2:2 10-bit HEVC playback via AVPlayer; MPVKit fallback if the media engine
   refuses it.
6. Video posters — currently disabled on iOS (no exec for ffmpeg); the plan's
   cgo libav shim is the fix.
