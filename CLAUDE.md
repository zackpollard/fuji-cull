# fuji-cull

One Go engine (`internal/cull`), four user interfaces on top of it. The engine
serves them all over the same HTTP API, so a feature is not "done" when the
engine can answer for it — it is done when every UI asks.

## The four UIs

| UI | Lives in | Notes |
|---|---|---|
| Desktop | `cmd/fuji-cull-gui` (Go + SDL) | **Linux and macOS are the same binary** — a desktop change is not a Linux change |
| Web | `internal/cull/ui/index.html` | one file, `//go:embed`ed into the engine; vanilla JS, no build step |
| Android | `android/` (Kotlin) | talks to the engine over HTTP, same as a remote client |
| iOS | `ios/` (Swift) + `mobile/` (gomobile bind) | runs the engine in-process |

## Carry every user-facing change across all four

When you add or change something a person can see — a field, an indicator, a
panel, a keybinding's effect — do it in all four UIs in the same change, or
state plainly in the PR which ones you skipped and why. "The engine exposes it"
is not shipping it.

This is not hypothetical bookkeeping. The sharpest-of-burst marker is the
worked example:

- the engine scores every frame and serves `GET /api/sharpness`, including the
  per-burst winners
- **iOS** draws it in the viewer, the grid and the timeline
- **desktop** had the drawing code and a `focusBest` map that nothing ever
  assigned — a nil map, so the marker could never appear, and nobody noticed
  because the code looked complete
- **web** and **Android** never call the endpoint at all

So the engine had identified 402 burst winners on a working card and exactly
one of four UIs showed them. Dead code that reads like a finished feature is
worse than an obvious gap, so prefer wiring a UI up to leaving a plausible stub.

Check parity by asking which endpoints each client actually calls, rather than
by reading for intent:

    grep -ohE '"api/[a-z]+' ios/FujiCull/*.swift | sort -u
    grep -ohE '/api/[a-z]+' android/app/src/main/java/pro/zackpollard/fujicull/*.kt | sort -u
    grep -a -oE '/?api/[a-z]+' internal/cull/ui/index.html | sort -u

The `-a` on that last one is required, not decoration. `index.html` uses literal
NUL bytes as JS sentinels (`let curMonth = "\0"`), so `file` calls it `data`
and grep treats it as binary — printing **nothing at all**, exit status 0, no
warning. Every search of the web UI is silently empty without it.

## Building

`ci.yml` builds each surface separately, and they fail independently:

| job | runs on | covers |
|---|---|---|
| `build` | ubuntu | `go vet ./...`, tests |
| `appimage` | ubuntu | desktop Linux bundle |
| `macos` | macos-14 | desktop macOS bundle |
| `ios` | macos-15 | `gomobile bind` + `xcodebuild` — compile only |
| `android` | ubuntu | Gradle build |

Nothing compiles the Swift or Kotlin locally on a typical dev machine, so CI is
the only check those get. A green `build` job says nothing about the mobile UIs.

## Verify against a real camera

The engine is mostly camera-shaped edge cases, and the hardware misbehaves in
ways no test reproduces: replayed transfer buffers that are the right length and
the wrong bytes, handles that rebind under you, a body that indexes only its
most recent ~30,000 objects. Run the app against a camera before claiming a
camera-facing change works — several bugs in this repo's history looked correct
in review and failed on contact.

Where a camera is not available, `--backend dir --root <dir>` reads a copied
DCIM tree instead, and `cmd/aft-sim` emulates the MTP surface.
