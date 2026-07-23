# Handoff: fuji-cull — cross-platform culling UI

## Overview
fuji-cull reviews 5,000–25,000 photos straight off a Fujifilm card: mark each shot **keep** or **reject** in one keystroke, then import only the winners to disk + an Immich server. One Go engine (camera protocol, prefetch, thumbnails, decisions, import) serves a small HTTP API; every client is a thin front-end over that API, so all four share the same nouns and states. This package is the design language plus the four surfaces built on it.

## About the design files
The files in this bundle are **design references authored in HTML** (as streaming "Design Components" — `.dc.html`). They are prototypes showing intended look and behavior, **not production code to copy**. Your task is to **recreate them in each target environment** using that platform's idioms and libraries:

- **Web UI** → the framework the real client uses (the prototype is plain HTML/JS).
- **Desktop** → Go + SDL (GPU stage), packaged as Linux AppImage + macOS .app.
- **Android** → Jetpack Compose.
- **iPad** → SwiftUI + UIKit.

Match the visuals pixel-closely; implement the behavior with native patterns (don't ship the HTML).

## Fidelity
**High-fidelity.** Final colors, typography, spacing, states, and interactions. The Web UI is a **working interactive prototype** (real keyboard cull loop, live counters, undo, viewer, video wall, all connect/import/settings/log states). Desktop/Android/iPad are annotated static per-screen mockups. Recreate pixel-closely using each codebase's component library.

## How to read the prototypes
Open `fuji-cull.dc.html` — it's the hub linking all five deliverables. Each opens in any browser.
- **`fuji-cull Design System.dc.html`** — tokens, the tile, every state, motion, a11y. The source of truth.
- **`fuji-cull Web UI.dc.html`** — the interactive flagship. A bottom-left **PROTO dock** switches between Connect / Video wall / Import / Settings / camera-sick / keymap so you can reach every state. Keyboard: arrows move, W/K keep, S/X reject, E clear, U undo, G next-undecided, Enter open viewer, Esc back, Z zoom, L pull video, I import, ? keymap.
- **`fuji-cull Desktop.dc.html`**, **`fuji-cull Android.dc.html`**, **`fuji-cull iPad.dc.html`** — annotated screens; amber **WHY** notes carry rationale.

---

## Design tokens

### Color (single dark theme — never a light mode)
| Token | Hex | Role | Contrast on --bg |
|---|---|---|---|
| `--bg` | `#0B0C0B` | App canvas (warm near-black; never pure black — OLED smear) | — |
| `--tile` | `#161815` | Tile / cell / input surface | — |
| `--surface` | `#1C1F1A` | Panels, sheets, filmstrip | — |
| `--surface-hi` | `#242821` | Hover, popover, active | — |
| `--line` | `#30352C` | Hairline border | — |
| `--text` | `#ECE9E0` | Primary text (warm cream) | 16.1:1 (AAA) |
| `--text-2` | `#9DA093` | Secondary text | 7.4:1 (AAA) |
| `--text-3` | `#6B6E62` | Muted / disabled | 3.8:1 (AA large) |
| `--amber` | `#FFB32E` | Cursor, primary action, focus | 11.0:1 (AAA) |
| `--keep` | `#38D67A` | Keep decision | 10.3:1 (AAA) |
| `--reject` | `#FF5A3D` | Reject decision | 6.3:1 (AA) |
| `--buffered` | `#4EA6FF` | Buffered-local dot (full-res cached) | 7.7:1 (AAA) |
| `--immich` | `#57C9C1` | In-Immich ring (already on server) | 9.8:1 (AAA) |

Additional darks used in chrome: `#0f110e` (inset panel), `#22261F` / `#17190f` (dividers), `#141613` (window titlebar).

### Typography — two metric-sibling families
- **IBM Plex Mono** — all *data*: counters, EXIF, filenames, keycaps, log tails, badges, section titles. Signals "engineered" and stays tabular.
- **IBM Plex Sans** — all *prose*: guidance, dialog copy, setting descriptions, WHY notes.
- **Identity decision (confirmed):** hybrid (mono data + sans prose), NOT all-mono. The design-system file shows current-vs-proposed side by side.

Scale (px / family / weight): Counter 28 Mono 600 · Title 19 Mono 500 uppercase +0.04em · Body 15 Sans 400 · Emphasis 15 Sans 600 · Label 13 Mono 500 · Micro 11 Mono 400. Slide/desktop text never below the readouts' 10–11px mono.

### Spacing & form
4px base (4 / 8 / 12 / 16 / 24 / 32). Radii tight — density is a feature: `0` grid & cells · `2` tile & chip · `4` button & input · `8` sheet & card · `999` dot & pill. Bezel radii larger on device frames only.

---

## The tile — core grammar (shared component)
Every shot is a 1:1 tile. See `Tile.dc.html` for the reference implementation and its prop list.

- **Decision bar — bottom edge, full width.** 6px solid `--keep` green or `--reject` red; undecided = 3px faint hairline (`rgba(255,255,255,0.08)`) so the slot reads "not yet decided," not empty. This edge is what makes a whole row's fates readable in one glance at 56px.
- **Cursor / focus — full inset ring.** 2px `--amber` outline, `outline-offset:-2px`, soft `0 0 14px rgba(255,179,46,0.28)` glow. Same ring is keyboard-focus everywhere (one focus language).
- **Buffered dot — top-right.** Solid `--buffered` blue, 9px, 1px dark halo. Full-res cached locally.
- **Immich ring — top-right.** Hollow 2px `--immich` teal ring, 9px. Already on the server. (Distinct *shape* from buffered so both survive shrinking.)
- **Video — centered play triangle** in a `rgba(11,12,11,0.5)` blurred disc.
- **RAF/JPG stack — top-left chip** `RAF`, 8px mono 600, on `rgba(11,12,11,0.72)`.
- **Thumb states:** `pending` = diagonal stripe skeleton (`repeating-linear-gradient(135deg,#141613,#191c17)`) + amber shimmer sweep; `failed` = `#131512` with a muted `×`.

Overlays are tiny and corner-pinned so they never fight the decision bar. **Nothing on a tile animates on data change** — a keep is an instant color swap on one bar (25k virtualized cells must stay smooth).

---

## Screens / views

### Top bar (web/desktop/iPad) — counter strip
Left: connected-device identity (green dot + `X-T5 · SD1`). Center: `K 8,204` (green) · `R 3,910` (red) · `U 12,146` (`--text-2`) in tabular mono, plus `th 4210/24260 · ex 3980/24260` sweep readout. Right: column stepper, `LOG`, `RESCAN`, amber `IMPORT · I`. Wraps rather than clips on narrow widths; counts use `K/R/U` compact form. If CAMERA SICK, an amber blinking chip sits inline.

### Grid / timeline (the cull view)
Month header (amber, +0.14em) → sticky day header (`WED 15 JUL` · `96 shots`) → dense `grid` of tiles, `gap:4px`, columns user-adjustable (4–14 web/desktop; 4 on phone; 9 on iPad). Right-edge **date scrubber** (see Interactions). Footer: keyboard legend (web/desktop/iPad) or nothing (touch relies on bottom buttons). Virtualize the list — target 25k items.

### Viewer / stage
Full-bleed shot on `--bg`; 3:2 stage with EXIF bottom-right (`1/500 f2.8 ISO 400` / `XF56mm · 28.4 MB`), decision chip top-center (KEEP/REJECT/UNDECIDED — color **and** text, never color-only). Below: filmstrip of 56–66px tiles centered on cursor. Web/Android/iPad show a three-up **REJECT / CLEAR / KEEP** button row (≥54px, full-width thumb targets); **desktop shows none** (keyboard-committed). Cross-dissolve + 8px slide between shots (120ms).

### Video (all surfaces)
4 GiB preview wall: only a preview streams from the card; overlay `PREVIEW ONLY · 4 GiB WALL` + "press **L** / pull the full clip." Pulling shows a `switching decoder` toast when a 4:2:2 clip falls back from hardware to software decode, then a scrubber + `✓ full clip pulled local`. Desktop/Android/iPad use mpv (hw 4:2:0 → sw 4:2:2 fallback); the fallback is surfaced as a chip so a stutter reads as known, not broken.

### Connect / discovery
States: looking for camera → PTP session opening → **indexing card** (percent, item counts, MB/s, ETA, ~3 min, live scanning-edge bar) → reading card index → ready. Plus **camera disconnected** and **wedged camera** ("power-cycle the camera") in reject-red, and CAMERA SICK (amber — USB link rested and recovering; culling continues on buffered shots). A live **log tail** is always one tap away. **No fake spinners** — every wait shows real numbers. On iPad this screen is a full telemetry dashboard (numbered phases + engine log); thumbnails stream in so culling can start before EXIF finishes.

### Import
Sheet/panel: destination folder, Immich album, file count + size (e.g. 8,204 · 214.6 GB), `START IMPORT`. Then four phase bars **copy → hash → upload → validate** (done = green ✓, active = amber with scanning edge + %, todo = grey), live `uploading 5,120 / 8,204 · 118 MB/s`, then a done state. Bottom sheet on Android, form sheet (centered card over dimmed grid) on iPad.

### Settings
Immich URL + API key, **Stack RAF + JPG** toggle (one tile per capture), import destination. **Force fake corpus** — developer-only, quarantined in reject-red with a ⚠ glyph and an "off before a real shoot" note (it hides the real camera and fills the grid with synthetic shots). No "Session name" field (see State).

### Log / diagnostics
Full engine log, monospace, color-coded (amber = link reset / decoder fallback, green = recovered, teal = Immich match). Copy/Share action. Right drawer (web), full screen (Android), etc.

---

## Interactions & behavior

### Keyboard model (identical: web, desktop, iPad-with-keyboard)
`← → ↑ ↓` navigate (geometric nearest-neighbor, not index math) · `W`/`K` keep · `S`/`X` reject (keep & reject **advance** to next) · `E`/`C` clear (no advance) · `U` undo last · `G` jump to next undecided (wraps) · `Enter` open viewer · `Esc` back/close · `Z` zoom 1:1 · `L` pull full video · `I` import · `R` retry · `?` keymap HUD. On iPad, holding ⌘ shows the shortcut HUD.

### Touch model (Android, iPad)
Tap tile → paged viewer; swipe pages shots. Decisions via the bottom REJECT/CLEAR/KEEP bar (≥48dp Android / ≥44pt iOS). Drag the right-edge scrubber to cross the card.

### Date scrubber
Right-edge, proportional to time on card (a heavy week takes more track than a quiet month). Fades in on scroll, fades out ~800ms–1.1s after idle. Labels **thin** (months → quarters → years) as the card grows so the count is bounded at 25k. Dragging shows a month/day bubble at the handle; release snaps the grid.

### Motion (durations / easing) — standard easing `cubic-bezier(0.2,0,0,1)`
Grid cells: **none** (instant). Viewer next/prev: cross-dissolve + 8px slide, 120ms ease-out. Zoom: scale about pointer, 160ms standard. Scrubber: fade-in 180ms / delayed fade-out 600ms. Sheet/dialog: slide-up 16px + scrim fade, 220ms standard. Progress bar: width tween 300ms linear, no overshoot. Decoder switch: toast 200ms. `prefers-reduced-motion` → every transition becomes an instant state change; the scrubber simply appears.

---

## State management
- **Decisions**: per-item `keep | reject | undecided`, with an **undo stack**. Counters (K/R/U) derive from this; keep/reject advance the cursor, clear does not.
- **Cursor**: current tile index; drives grid focus, viewer, filmstrip. Keep visible on nav (scroll into view without `scrollIntoView`).
- **Autosave**: decisions persist server-side continuously — **never show a save button, never lose a decision.** A crash, disconnect, or switching devices resumes exactly where you left off.
- **Sessions (internal only):** the engine keys persisted decisions to a session that is **one-per-camera/card, created automatically, never user-named or user-switchable.** Do **not** build a session picker or a "Session name" setting. The UI only ever shows the connected **camera/card** identity (e.g. `X-T5 · SD1`) as a findable label. *(This overrides the original brief, which described user-named sessions — confirmed changed with the design owner.)*
- **Connect/engine status**: indexing %, thumb/exif sweep counts, throughput, camera-sick flag, decoder mode — all from the API, shown honestly.
- **Buffered window**: which shots are cached full-res locally (drives the blue dot and instant viewer).

## Assets
No raster assets required. The **logo** is pure CSS/SVG: an amber (`--amber`) triangle "mountain" with a cream (`--text`) inner notch and a `--keep` green baseline bar, on a `--tile` rounded square — recreate as a vector. Photo thumbnails in the prototypes are CSS-gradient placeholders standing in for real JPEG thumbnails from the engine. Fonts: IBM Plex Mono + IBM Plex Sans (Google Fonts).

## Files in this bundle
- `fuji-cull.dc.html` — hub / index (open first)
- `fuji-cull Design System.dc.html` — tokens, tile, states, motion, a11y (source of truth)
- `fuji-cull Web UI.dc.html` — interactive flagship prototype
- `fuji-cull Desktop.dc.html` — SDL desktop mockups
- `fuji-cull Android.dc.html` — Jetpack Compose mockups
- `fuji-cull iPad.dc.html` — SwiftUI/UIKit mockups
- `Tile.dc.html` — shared tile component (reference for the grammar)
- `support.js` — runtime for the `.dc.html` files (required to open them; not app code)
- `android-frame.jsx` — device-bezel scaffold for the Android mockups (presentation only; not app code)

To view: open any `.dc.html` in a browser (they load fonts from Google Fonts + `support.js` locally).
