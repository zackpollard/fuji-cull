# aft-mtp-cli partial-read patch

`partial-reads.patch` adds two commands to
[android-file-transfer-linux](https://github.com/whoozle/android-file-transfer-linux)
(pinned to the commit in `UPSTREAM_COMMIT`):

- `get-part <id> <offset> <size> <dest>` — MTP GetPartialObject to a file
- `serve-parts` — persistent partial-read server: `R <id> <offset> <size>`
  on stdin answers `\x01OK <n>` plus n raw bytes on stdout; `Q` quits
- `--device-fd N` — operate on a pre-opened usbfs file descriptor instead of
  scanning /dev/bus/usb (Android USB Host API hands apps an fd)

fuji-cull uses these for EXIF header sweeps, thumbnail healing, video
posters and camera video streaming. The AppImage build applies this patch
and ships the resulting binary as `aft-mtp-cli-part`.
