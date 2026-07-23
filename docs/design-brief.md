# fuji-cull — design brief

You are designing the interfaces for **fuji-cull**, a photo-culling tool for
Fujifilm cameras. Read this whole brief before designing: it describes the
product, every interface, every state the UI must express, and the hard
engineering constraints your designs must respect.

## The product in one paragraph

A photographer comes home with 5,000–25,000 photos and videos on a camera
card. fuji-cull lets them review **straight off the camera** — nothing is
copied until they decide. They connect the camera over USB, get a timeline of
the whole card in ~3 minutes, fly through shots at full resolution, mark
keep/reject with single keystrokes or taps, watch videos in place, and then
import only the keepers (to disk and optionally to an Immich photo server).
The job is *speed with confidence*: thousands of decisions in one sitting,
often in a dim room after a shoot, without ever wondering "did that save?".

## Architecture (why the UIs look related)

One Go engine does everything (camera protocol, prefetching, thumbnails,
sessions, import) and serves a small HTTP API. Every interface is a thin
client of that API, so they all share the same nouns and states. Decisions
persist server-side per named session — a crash, disconnect, or switching
devices resumes exactly where you left off.

## Current visual identity

- **Palette**: near-black charcoal background `#0B0C0B`; tile grey `#161815`;
  amber accent `#FFB32E` (cursor, headers, primary actions); keep-green
  `#38D67A`; reject-red `#FF5A3D`; secondary text grey.
- **Type**: monospaced throughout (terminal heritage; the audience likes it).
- **Tile grammar**: every shot is a thumbnail tile. A **decision bar** across
  the tile's bottom edge shows its fate (green = keep, red = reject, none =
  undecided). Small overlays: blue dot = buffered locally, cloud glyph =
  already in Immich, play badge = video, amber outline = cursor.
- **Logo**: amber Mount Fuji with a cream snowcap on a charcoal tile, resting
  on a keep-green decision bar (the tile grammar as identity).

You may evolve all of this — but the tile grammar's glanceability (decision
state readable at 60px, in a 10-column grid, at arm's length) is the core UX
and must survive any redesign.

## The four interfaces

### 1. Web UI (browser, served by the engine)

Keyboard-first, single page. Elements today: top bar with keep/reject/
undecided counters, thumbnail+EXIF progress ("th 4210/24260 · ex …"),
CAMERA SICK warning, RESCAN and IMPORT buttons, column-count stepper; a
virtualized thumbnail **grid** with month/day headers and a right-edge
**date scrubber** (proportional month labels, draggable handle, month
bubble); a full-screen **stage** for the current shot (canvas, zoom/pan,
1:1 toggle) with filename, position counter, BUFFERING state, error
overlay; a **filmstrip** under the stage; a video player with a
preview-limit wall ("to keep watching, pull the full clip — L");
an **import panel** (destination, album, file/size/shot counts, phase
progress bar, per-phase status, errors). Keys: arrows navigate, W/K keep,
S/X reject, E/C clear, U undo, G next undecided, Z zoom, L load video,
R retry, I import.

### 2. Desktop GUI (native, SDL — Linux AppImage + macOS app)

The same engine in-process with a GPU-rendered stage for speed (all-core
JPEG decode). Deliberately minimal chrome: grid view and stage view, same
keyboard model as the web UI, mpv-based video playback, import panel.
Design for zero-mouse operation; the pointer is optional everywhere.

### 3. Android app (Jetpack Compose, tablets and phones)

Screens: **Connect** (engine/discovery status, live log tail, guidance
"set the camera to USB card-reader mode", settings/log links);
**Cull screen** = header bar + virtualized timeline grid (month sections,
day headers) + Immich-style fade-in **timeline scrubber**; **Viewer** =
paged full-screen shots, pinch-zoom images, mpv video player (hardware
4:2:0, auto-fallback software 4:2:2) with pause/seek/buffer UI, filmstrip,
decision buttons (REJECT / CLEAR / KEEP); **Import dialog** (destination,
album, progress phases); **Settings** (Immich URL/key, RAF+JPG stacking,
session name, import destination); **Log screen** (diagnostics, share).

### 4. iPad app (SwiftUI + UIKit, the newest surface)

Same screen set as Android with iPad idioms: **Connect** screen doubles as
camera-bring-up telemetry (the ~3-minute indexing wait shows live progress
percent and a log tail — design this wait to feel purposeful, it happens
every connection); UICollectionView timeline with day/month headers and the
fade-in date scrubber; UIPageViewController viewer with filmstrip, decision
bar, tap-to-pause mpv video; hardware-keyboard support (same key map as
desktop); Import and Settings sheets (Settings includes a deliberately
scary "Force fake corpus" developer toggle — make it look like one).

## States every design must express

- **Connect/discovery**: looking for camera → session opening → camera
  indexing (percent, ~3 min) → reading card index → ready. Also: camera
  disconnected, wedged camera ("power-cycle the camera"), engine start
  failure. These are long, real waits — design them honestly (progress,
  logs on demand), not with fake spinners.
- **Per-shot**: undecided / keep / reject; buffered; in-Immich; video;
  thumbnail pending vs loaded vs failed; cursor.
- **Global**: keep/reject/undecided counts; thumbnail + EXIF sweep progress;
  CAMERA SICK (the camera's USB link is returning garbage and is being
  rested — a warning state users must notice but not panic over); import
  running (phase: copy/hash/upload/validate + n/total) / done / error.
- **Video**: poster loading; playing (hardware or software decode — the
  first seconds of a 4:2:2 clip may freeze then recover; consider signaling
  "switching decoder"); buffering; the 4 GiB preview wall ("only a preview
  streams; press L / pull for the full clip"); pulled-local state.
- **Sessions**: named session shown somewhere findable; decisions autosave —
  never show a save button, never lose one.

## Hard constraints

- **Scale**: 25,000+ items. Every list/grid/strip is virtualized; cells are
  value-driven and cheap. No design may require per-cell animation on data
  change, whole-list re-layout, or unbounded label counts (the scrubber
  thins its labels; keep that).
- **Glanceability over chrome**: users sweep their eyes across 60–120 tiles
  at a time. Decision state must read peripherally. Density is a feature —
  don't pad it away.
- **Dark, always**: culling happens in dim rooms next to a monitor showing
  the actual photos. Single dark theme; avoid pure black smearing on OLED;
  avoid bright surfaces entirely.
- **Keyboard-first on desktop/web/iPad-with-keyboard**; touch-first on
  Android/iPad. Every keyboard action needs a touch equivalent and vice
  versa.
- **Honest progress**: this app's credibility rests on never lying about
  camera state. Waits are shown with real numbers (percent, counts,
  MB/s where known) and a path to diagnostics (log tail is one tap away).
- **No network assumptions**: everything is local except optional Immich.

## What we want from you

1. A **cross-platform design system**: tokens (color, type scale — keep or
   deliberately replace the monospace identity, spacing, radii), the tile
   component with all overlay states, buttons, progress, badges, sheets.
2. **Per-screen redesigns** for all four surfaces (connect, grid/timeline,
   viewer incl. video, import, settings, log/diagnostics), with rationale.
   Platform-idiomatic variation is welcome; shared grammar is required.
3. **Motion guidelines** compatible with virtualization (transitions on the
   viewer/scrubber/sheets — not on grid cells).
4. **The waits**: make connect/index and import feel engineered, not broken.
5. Accessibility: contrast targets on the dark palette, touch-target sizes,
   Dynamic Type strategy where the platform supports it.

Deliver as annotated mockups per screen plus the component inventory. Where
you diverge from the current identity, show the current vs proposed and say
why.
