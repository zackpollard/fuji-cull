import AVKit
import SwiftUI

// ViewerView is the full-screen culling surface: swipe between frames, pinch to
// zoom, KEEP/REJECT with auto-advance. Paging updates the engine cursor so the
// prefetch window follows. Mirrors the Android/web viewer against the same API.
struct ViewerView: View {
    @ObservedObject var model: GridModel
    @Binding var index: Int
    @Environment(\.dismiss) private var dismiss
    @State private var showKeymap = false

    private var shot: Shot? { model.shots.indices.contains(index) ? model.shots[index] : nil }
    private var decision: String { shot.flatMap { model.decisions[$0.id] } ?? "" }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            // UIPageViewController pager (PagerView): natively virtualized —
            // the previous windowed TabView mutated its page set while a
            // swipe settled and stranded the pager halfway between pages.
            PagerView(model: model, index: $index)
                .ignoresSafeArea()

            VStack(spacing: 0) {
                topBar
                decisionChip.padding(.top, DS.s2)
                Spacer()
                Filmstrip(model: model, index: $index)
                bottomBar
            }

            if showKeymap {
                keymapHUD
                    .onTapGesture { showKeymap = false }
            }
        }
        .statusBarHidden()
        .onChange(of: index) { i in model.select(i) }
        .keyCommands { key in
            switch key {
            case "k", "w": decide("keep"); return true
            case "x", "s": decide("reject"); return true
            case "c", "e": decide("clear"); return true
            case "right", "enter", " ": advance(); return true
            case "left": if index > 0 { withAnimation { index -= 1 } }; return true
            case "?", "/": showKeymap.toggle(); return true
            case "esc":
                if showKeymap { showKeymap = false } else { dismiss() }
                return true
            default: return false
            }
        }
    }

    private var topBar: some View {
        HStack {
            Button { dismiss() } label: {
                Image(systemName: "xmark").font(.title3.weight(.semibold))
            }
            Spacer()
            if let shot {
                Text("\(shot.folder) / \(shot.base)")
                    .font(.system(.subheadline, design: .monospaced))
            }
            Spacer()
            Text("\(index + 1)/\(model.shots.count)")
                .font(.system(.subheadline, design: .monospaced))
                .foregroundStyle(.secondary)
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 20)
        .padding(.top, 14)
        .background(LinearGradient(colors: [.black.opacity(0.6), .clear], startPoint: .top, endPoint: .bottom))
    }

    private var bottomBar: some View {
        // design: three-up full-width thumb targets, ≥54pt
        HStack(spacing: DS.s3) {
            decisionButton(title: "REJECT", system: "xmark.circle.fill",
                           tint: DS.reject, active: decision == "reject") { decide("reject") }
            decisionButton(title: "CLEAR", system: "circle.slash",
                           tint: DS.text2, active: decision == "") { decide("clear") }
            decisionButton(title: "KEEP", system: "checkmark.circle.fill",
                           tint: DS.keep, active: decision == "keep") { decide("keep") }
        }
        .padding(.horizontal, DS.s4)
        .padding(.vertical, DS.s3)
        .frame(maxWidth: .infinity)
        .background(LinearGradient(colors: [.clear, DS.bg.opacity(0.85)], startPoint: .top, endPoint: .bottom))
    }

    private func decisionButton(title: String, system: String, tint: Color, active: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: DS.s2) {
                Image(systemName: system).font(.system(size: 20))
                Text(title).font(DS.label(13))
            }
            .foregroundStyle(active ? DS.bg : tint)
            .frame(maxWidth: .infinity, minHeight: 54)
            .background(active ? tint : DS.surface, in: RoundedRectangle(cornerRadius: DS.rControl))
            .overlay(RoundedRectangle(cornerRadius: DS.rControl).strokeBorder(active ? .clear : DS.line))
        }
        .buttonStyle(.plain)
    }

    /// Decision chip on the stage, top-center — color AND text, never
    /// color-only.
    private var decisionChip: some View {
        let (label, tint): (String, Color) =
            decision == "keep" ? ("KEEP", DS.keep)
            : decision == "reject" ? ("REJECT", DS.reject)
            : ("UNDECIDED", DS.text3)
        return Text(label)
            .font(DS.label(12))
            .foregroundStyle(decision.isEmpty ? DS.text2 : DS.bg)
            .padding(.horizontal, DS.s3).padding(.vertical, 5)
            .background(decision.isEmpty ? DS.surface : tint,
                        in: RoundedRectangle(cornerRadius: DS.rControl))
    }

    /// The keymap HUD (design: `?` toggles; one look at the whole model).
    private var keymapHUD: some View {
        let rows: [(String, String)] = [
            ("← → ↑ ↓", "navigate"), ("W / K", "keep (advances)"),
            ("S / X", "reject (advances)"), ("E / C", "clear"),
            ("G", "next undecided"), ("Z", "zoom 1:1"),
            ("L", "pull full video"), ("Enter", "open viewer"),
            ("Esc", "back"), ("?", "this keymap"),
        ]
        return VStack(alignment: .leading, spacing: DS.s2) {
            Text("KEYMAP").font(DS.title(14)).foregroundStyle(DS.amber)
            ForEach(rows, id: \.0) { r in
                HStack {
                    Text(r.0).font(DS.label(13)).foregroundStyle(DS.text)
                        .frame(width: 110, alignment: .leading)
                    Text(r.1).font(DS.body(13)).foregroundStyle(DS.text2)
                }
            }
        }
        .padding(DS.s5)
        .background(DS.surface, in: RoundedRectangle(cornerRadius: DS.rSheet))
        .overlay(RoundedRectangle(cornerRadius: DS.rSheet).strokeBorder(DS.line))
    }

    private func decide(_ d: String) {
        guard let shot else { return }
        model.setDecision(shot, d == "clear" ? "" : d)
        if d != "clear" { advance() }
    }

    private func advance() {
        if index < model.shots.count - 1 {
            withAnimation { index += 1 }
        }
    }
}

// VideoFrame plays a clip straight off the camera via the engine's /api/video
// range server, through libmpv (MpvPlayerView) — AVPlayer could not sustain
// this card's footage: it froze Rext (4:2:2 10-bit) clips seconds in with a
// full buffer and starved everything else in a 64 KB-per-request crawl, the
// same class of platform-player failure that pushed Android onto libmpv.
struct VideoFrame: View {
    @ObservedObject var model: GridModel
    let shot: Shot
    let active: Bool

    @State private var decoderToast = false

    var body: some View {
        ZStack {
            Color.black
            if active {
                MpvPlayerView(model: model, shot: shot)
            } else {
                ProgressView().tint(.white)
            }
            if decoderToast {
                Text("SWITCHING DECODER · SOFTWARE")
                    .font(DS.label(12))
                    .foregroundStyle(DS.bg)
                    .padding(.horizontal, DS.s3).padding(.vertical, 6)
                    .background(DS.amber, in: RoundedRectangle(cornerRadius: DS.rControl))
                    .transition(.opacity)
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: MpvPlayerView.decoderSwitched)) { _ in
            guard active else { return }
            withAnimation(DS.easing) { decoderToast = true }
            Task {
                try? await Task.sleep(nanoseconds: 2_500_000_000)
                withAnimation(DS.easing) { decoderToast = false }
            }
        }
    }
}

// Filmstrip is the horizontal thumbnail strip under the frame: current one
// highlighted, decisions marked, tap to jump; auto-scrolls to the cursor.
//
// Windowed (±25 shots), like every other 24k surface in this app: a
// full-catalog LazyHStack rebuilt a 24k array per render — and LazyHStack is
// greedy on its cross axis, so the strip filled all the height the layout
// offered and floated its tiles at mid-screen. A fixed height belts the
// braces.
struct Filmstrip: View {
    @ObservedObject var model: GridModel
    @Binding var index: Int

    private var window: [Int] {
        guard !model.shots.isEmpty else { return [] }
        let lo = max(0, index - 25), hi = min(model.shots.count - 1, index + 25)
        return Array(lo...hi)
    }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 3) {
                    ForEach(window, id: \.self) { i in
                        let s = model.shots[i]
                        let decision = model.decisions[s.id] ?? ""
                        // videos use the client-side poster (the engine never
                        // has a thumb for them on mobile)
                        let poster = s.kind == "video" ? Posters.shared.cached(s) : nil
                        ZStack(alignment: .bottom) {
                            if let poster {
                                ThumbView(url: poster, cacheKey: "\(s.id):poster", ready: true)
                            } else if let url = model.thumbURL(s.id, i) {
                                ThumbView(url: url, cacheKey: "\(s.id):\(model.orientOf(i))", ready: model.thumbReady(i),
                                          exifOrient: model.orientOf(i))
                            } else {
                                Color.white.opacity(0.05)
                            }
                            if decision == "keep" || decision == "reject" {
                                Rectangle()
                                    .fill(decision == "keep" ? Color(red: 0.22, green: 0.84, blue: 0.48) : Color(red: 1.0, green: 0.35, blue: 0.24))
                                    .frame(height: 3)
                            }
                        }
                        .frame(width: 64, height: 44)
                        .clipped()
                        .overlay(Rectangle().stroke(i == index ? Color(red: 1.0, green: 0.70, blue: 0.18) : .clear, lineWidth: 2))
                        .id(i)
                        .onTapGesture { withAnimation { index = i } }
                    }
                }
                .padding(.horizontal, 8)
                .padding(.vertical, 6)
            }
            .frame(height: 56)
            .background(DS.surface.opacity(0.92))
            .onChange(of: index) { i in withAnimation { proxy.scrollTo(i, anchor: .center) } }
            .onAppear { proxy.scrollTo(index, anchor: .center) }
        }
    }
}

// ZoomableImage loads a full frame from /api/image (the engine demand-fetches
// and blocks until ready) and supports pinch-zoom + double-tap.
struct ZoomableImage: View {
    let url: URL?
    // holds the shared zoom so advancing to the next shot keeps the level —
    // the desktop comparison workflow ("same crop, next frame") on touch
    var model: GridModel? = nil
    @State private var image: UIImage?
    @State private var loadFailed = false
    @State private var scale: CGFloat = 1
    @GestureState private var pinch: CGFloat = 1

    var body: some View {
        GeometryReader { geo in
            ZStack {
                if let image {
                    Image(uiImage: image)
                        .resizable()
                        .scaledToFit()
                        .scaleEffect(scale * pinch)
                        .gesture(
                            MagnificationGesture()
                                .updating($pinch) { v, s, _ in s = v }
                                .onEnded { v in
                                    scale = min(max(scale * v, 1), 5)
                                    model?.viewerZoom = scale
                                }
                        )
                        .onTapGesture(count: 2) {
                            withAnimation { scale = scale > 1 ? 1 : 2.5 }
                            model?.viewerZoom = scale
                        }
                } else if loadFailed {
                    Image(systemName: "exclamationmark.triangle")
                        .font(.system(size: 40)).foregroundStyle(.orange)
                } else {
                    ProgressView().tint(.white)
                }
            }
            .frame(width: geo.size.width, height: geo.size.height)
        }
        .task(id: url) { await load() }
    }

    private func load() async {
        // inherit the shared zoom instead of resetting: paging to the next
        // shot keeps the magnification you were comparing at
        image = nil; loadFailed = false; scale = model?.viewerZoom ?? 1
        guard let url,
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let img = UIImage(data: data) else { loadFailed = true; return }
        image = img
    }
}
