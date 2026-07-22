# fuji-cull — iPad app

Native SwiftUI client of the fuji-cull engine (the same Go core the desktop and
Android apps run), driving it over a loopback HTTP API. See
`docs/ipad-plan.md` for the architecture.

## Build & run (simulator)

Requires `go`, `gomobile`, `xcodegen`, and Xcode.

```sh
./run-sim.sh --bind          # gomobile bind engine → Mobile.xcframework, then build+run
./run-sim.sh                 # rebuild+run (engine unchanged)
./run-sim.sh --clean         # also wipe app Documents (re-seed the fake corpus)
./run-sim.sh --shot out.png  # screenshot after launch
```

`Mobile.xcframework` (gomobile output) and `FujiCull.xcodeproj` (xcodegen
output) are generated, not committed — `run-sim.sh` produces both.

## Backends

- **Simulator**: `StartLocal` runs the engine against the `dir` backend over a
  synthetic corpus (`SeedFakeCorpus`) — no camera, no exec.
- **Device**: `StartICC` runs against `ICCTransport` (ImageCaptureCore) talking
  to the tethered X-H2S. The app prefers the camera when one is attached and
  falls back to the fake corpus otherwise (so the simulator always works).

## Layout

- `project.yml` — xcodegen spec (deployment target iOS 16, downgraded to Xcode
  15.4's project format by `run-sim.sh`).
- `FujiCull/*.swift` — app: Engine wrapper, Connect/Grid/Viewer/Import/Settings,
  API client, ICCTransport.
- engine facade: `../mobile/ios.go` (`StartLocal`, `StartICC`, `SeedFakeCorpus`).
