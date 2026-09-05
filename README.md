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

Immich credentials can also be entered in the app — `⌘,` (or the cog in the
header) on macOS, Settings on iOS/Android — and are stored in
`~/.local/share/fuji-cull/import-defaults.json` (mode `0600`). This matters on
macOS: an app launched from Finder inherits no shell environment, so
`IMMICH_URL`/`IMMICH_API_KEY` exported in your shell are invisible to it.

**API key permissions.** Create the key in Immich with:

| Permission | Needed for |
| --- | --- |
| `asset.upload` | always — the upload itself and the bulk-upload-check that detects shots already on the server |
| `album.read`, `album.create`, `albumAsset.create` | only when importing into an album |
| `stack.create` | only with RAF+JPG stacking enabled |

An under-scoped key still saves fine and only fails at import time, so it is
worth ticking these when you create it.

Decisions persist per `--session`, so a disconnect or restart resumes where
you left off. Use `--listen 0.0.0.0:8787` to cull from another device.
`--backend dir --root <dir>` works against any local directory with
`NNN_FUJI` folders (handy for testing); `--camera-root` overrides the
`/SLOT 1/DCIM,/SLOT 2/DCIM` default if the camera exposes storage differently.

### Remote camera host (browse from any device)

Plugging the camera into the iPad or phone can be awkward. Instead, plug it
into one machine (your desktop, or an always-on box) and browse/cull/import
from anywhere on the LAN — the camera host does all the work, every other
device is just a viewport.

On the host, run the engine bound to the network **with a key** (required to
expose it safely — leave it off and it stays loopback-only as before):

```sh
fuji-cull --listen 0.0.0.0:8787 --engine-key "$(openssl rand -hex 24)"
# or env FUJI_ENGINE_KEY=…
```

Then connect from each device:

- **Browser (any device):** open `http://<host>:8787` and enter the key once.
- **iPad / Android:** Settings → *Camera source* → the host URL + key.

The key authenticates every request; media is fetched with it too. This is a
**LAN** feature — for access beyond a trusted network, front it with a TLS
reverse proxy or a private network (Tailscale/WireGuard). It composes with
cross-device sync but doesn't need it: thin clients share the one host engine,
so they always see the same state.

## fuji-cull for macOS (desktop app)

Each release ships `fuji-cull-macos-arm64.dmg` (Apple Silicon; there is no
Intel build). The bundle carries everything it shells out to — `gphoto2` and
its libgphoto2 drivers, `ffmpeg`, the patched `aft-mtp-cli`, SDL — so there
is no Homebrew step. Open the dmg and drag `fuji-cull.app` to
`/Applications`.

### First launch: "Apple could not verify …"

There's no paid Apple developer account behind this app, so the bundle is
**ad-hoc signed but not notarized**. macOS quarantines anything downloaded
from the internet, and with no Apple signature to check the app against,
Gatekeeper blocks the first launch with:

> **"fuji-cull" Not Opened** — Apple could not verify "fuji-cull" is free of
> malware that may harm your Mac or compromise your privacy.

That is expected, not a broken download. Two ways past it:

**Terminal (one command, recommended).** Clears the quarantine flag from the
app *and* the helper tools inside it:

```sh
xattr -dr com.apple.quarantine /Applications/fuji-cull.app
```

**Or through System Settings.** Double-click the app and dismiss the dialog
with **Done** — *not* Move to Trash. Then open **System Settings ▸ Privacy &
Security**, scroll down to Security, and click **Open Anyway** next to the
line about fuji-cull; confirm in the dialog that follows and authenticate.
That button only appears after a blocked launch attempt, so open the app
first. macOS may ask again for `gphoto2` or `ffmpeg` the first time you
connect a camera — the `xattr` command above is the one that covers the
bundled helpers too.

Right-click ▸ **Open** is the old advice for this and no longer works: macOS
15 (Sequoia) removed that bypass, so on current macOS use one of the two
routes above.

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

## Cross-device sync (fuji-sync)

Start culling on one device and resume on another. Each client keeps a full
local copy of its keep/reject decisions and syncs them, when online, through a
small **self-hosted** relay you run — so no decision ever lives only on the
server. Decisions are keyed per camera (model + serial), so the same card's
progress follows you across the iPad, phone, desktop, and web UI.

**Run the server** (Docker):

```sh
cd deploy/sync
echo "SYNC_API_KEY=$(openssl rand -hex 32)" > .env      # a strong shared key
docker compose up -d --build                            # listens on :8777
```

Put it behind your own TLS reverse proxy for anything beyond a trusted LAN — the
API key is the only credential. (Or run the binary directly:
`go run ./cmd/fuji-sync --api-key <key> --db ./sync.db`.)

**Point each client at it:**

- **Desktop:** `fuji-cull --sync-url https://sync.example.com --sync-key <key>`
  (or `FUJI_SYNC_URL` / `FUJI_SYNC_KEY` env).
- **iPad / Android:** Settings → *Cross-device sync* → server URL + key.
- **Web:** it's served by the engine, so it uses whatever the engine process was
  started with.

Sync is entirely optional and inert until configured. It's offline-first (queues
your decisions and retries forever), merges concurrent edits per photo by
most-recent-wins with tombstones for clears, and never overwrites a genuine edit
with an older one. Design notes: [`docs/sync-design.md`](docs/sync-design.md).

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
