# fuji-tools

Tools for getting photos off a Fuji X camera (X-H2S) over MTP.

## fuji-cull

Web UI for culling photos **straight off the camera** — nothing is copied
until you decide. Camera access uses `aft-mtp-cli` batch pulls (FUSE MTP
mounts don't work against the X-H2S; the cli does). A sliding window of
full-size previews around your cursor is buffered to local NVMe so browsing
is instant. Kept shots (RAF+JPG pairs move together) are then pulled to the
destination and pushed through the import pipeline. RAF-only shots pull the
RAF once and show its embedded full-res preview; videos are only pulled on an
explicit keypress.

```sh
fuji-cull --dest '/mnt/skynas/.../2026-07-06' \
          --immich-url https://immich.example.com --immich-key XXX \
          --immich-album 'Trip 2026'
# open http://127.0.0.1:8787
```

Keys: `←→` navigate · `K` keep · `X` reject · `C` clear · `U` undo ·
`Z`/click 100% zoom · `G` next undecided · `L` load video · `R` retry ·
`I` import.

Decisions persist per `--session`, so a disconnect or restart resumes where
you left off. Use `--listen 0.0.0.0:8787` to cull from another device.
`--backend dir --root <dir>` works against any local directory with
`NNN_FUJI` folders (handy for testing); `--camera-root` overrides the
`/SLOT 1/DCIM,/SLOT 2/DCIM` default if the camera exposes storage differently.

## fuji-cull for iPad (iOS app)

<img src="assets/fuji-cull.png" width="96" align="right">

The same engine as a native iPad app: plug the camera into the iPad's USB-C
port (camera in USB card-reader mode), get the full timeline in ~3 minutes,
cull with taps or a keyboard, play videos (4:2:0 hardware / 4:2:2 software
via mpv), and import keepers to Immich. Uses Apple's ImageCaptureCore for
camera access — no jailbreak, no special cables.

### Installing (no App Store)

There's no paid Apple developer account behind this app, so it ships as an
**unsigned `.ipa`** that you sign onto your own device with your own free
Apple ID via [SideStore](https://sidestore.io) (recommended) or
[AltStore](https://altstore.io).

One-time setup with SideStore:

1. Follow the [SideStore install guide](https://docs.sidestore.io/docs/intro)
   — you'll need a Mac or PC once, to install SideStore itself and to
   generate the device **pairing file**.
2. Install a loopback VPN for on-device refresh — **StikDebug** or
   **LocalDevVPN** (StosVPN also works if you already have it; it left the
   App Store).
3. In SideStore, sign in with your (free) Apple ID, then add this source:

   ```
   https://raw.githubusercontent.com/zackpollard/fuji-cull/master/ios/sidestore-source.json
   ```

4. Install **fuji-cull** from the source. Updates appear in SideStore.

Or skip the source and grab `FujiCull.ipa` from the
[latest release](https://github.com/zackpollard/fuji-cull/releases) and open
it with SideStore/AltStore directly.

Free-Apple-ID limits apply (Apple's, not ours): app signatures last 7 days
(SideStore auto-refreshes on-device with the VPN enabled), and a device can
hold at most 3 sideloaded apps.

First launch: iOS may ask you to trust your certificate under
Settings ▸ General ▸ VPN & Device Management, and to allow the camera
accessory when you first plug it in.

### Building it yourself

With a Mac + Xcode and a free Apple ID (7-day signing, no tools needed):

```sh
cd ios && ./run-device.sh --bind    # build engine + app, install to your iPad
./make-ipa.sh --bind                # or: produce dist/FujiCull.ipa
```

Video playback uses [MPVKit](https://github.com/mpvkit/MPVKit) (mpv/FFmpeg,
LGPL build).

## fuji-import

Headless bulk importer (the original tool): pull everything via `aft-mtp-cli`,
mirror to disk, restamp mtimes from EXIF, SHA-1, upload to Immich, validate by
checksum with retries.

```sh
fuji-import --dest /path --immich-url ... --immich-key ... --immich-album '...'
```

## Requirements

`android-file-transfer` (aft-mtp-cli / aft-mtp-mount), `perl-image-exiftool`.

## Build

```sh
go build ./cmd/fuji-cull ./cmd/fuji-import
```
