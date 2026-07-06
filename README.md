# fuji-tools

Tools for getting photos off a Fuji X camera (X-H2S) over MTP.

## fuji-cull

Web UI for culling photos **straight off the camera** — nothing is copied
until you decide. The camera is mounted via `aft-mtp-mount` (FUSE); a sliding
window of full-size previews around your cursor is buffered to local NVMe so
browsing is instant. Kept shots (RAF+JPG pairs move together) are then pulled
to the destination and pushed through the import pipeline.

```sh
fuji-cull --dest '/mnt/skynas/.../2026-07-06' \
          --immich-url https://immich.example.com --immich-key XXX \
          --immich-album 'Trip 2026'
# open http://127.0.0.1:8787
```

Keys: `←→` navigate · `K` keep · `X` reject · `C` clear · `U` undo ·
`Z`/click 100% zoom · `G` next undecided · `R` retry fetch · `I` import.

Decisions persist per `--session`, so a disconnect or restart resumes where
you left off. Use `--listen 0.0.0.0:8787` to cull from another device.
`--no-mount --mount-dir <dir>` works against any local directory with
`NNN_FUJI` folders (handy for testing).

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
