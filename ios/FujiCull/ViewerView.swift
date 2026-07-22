import AVKit
import SwiftUI

// ViewerView is the full-screen culling surface: swipe between frames, pinch to
// zoom, KEEP/REJECT with auto-advance. Paging updates the engine cursor so the
// prefetch window follows. Mirrors the Android/web viewer against the same API.
struct ViewerView: View {
    @ObservedObject var model: GridModel
    @Binding var index: Int
    @Environment(\.dismiss) private var dismiss

    private var shot: Shot? { model.shots.indices.contains(index) ? model.shots[index] : nil }
    private var decision: String { shot.flatMap { model.decisions[$0.id] } ?? "" }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            TabView(selection: $index) {
                ForEach(Array(model.shots.enumerated()), id: \.element.id) { i, s in
                    Group {
                        if s.kind == "video" {
                            VideoFrame(model: model, shot: s, active: i == index)
                        } else {
                            ZoomableImage(url: model.imageURL(s.id))
                        }
                    }
                    .tag(i)
                }
            }
            .tabViewStyle(.page(indexDisplayMode: .never))
            .ignoresSafeArea()

            VStack(spacing: 0) {
                topBar
                Spacer()
                Filmstrip(model: model, index: $index)
                bottomBar
            }
        }
        .statusBarHidden()
        .onChange(of: index) { i in model.select(i) }
        .keyCommands { key in
            switch key {
            case "k", "w": decide("keep"); return true
            case "x", "s": decide("reject"); return true
            case "c": decide("clear"); return true
            case "right", "enter", " ": advance(); return true
            case "left": if index > 0 { withAnimation { index -= 1 } }; return true
            case "esc": dismiss(); return true
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
        HStack(spacing: 36) {
            decisionButton(title: "REJECT", system: "xmark.circle.fill",
                           tint: Color(red: 1.0, green: 0.35, blue: 0.24),
                           active: decision == "reject") { decide("reject") }
            decisionButton(title: "CLEAR", system: "circle.slash",
                           tint: .white.opacity(0.7),
                           active: decision == "") { decide("clear") }
            decisionButton(title: "KEEP", system: "checkmark.circle.fill",
                           tint: Color(red: 0.22, green: 0.84, blue: 0.48),
                           active: decision == "keep") { decide("keep") }
        }
        .padding(.vertical, 18)
        .frame(maxWidth: .infinity)
        .background(LinearGradient(colors: [.clear, .black.opacity(0.65)], startPoint: .top, endPoint: .bottom))
    }

    private func decisionButton(title: String, system: String, tint: Color, active: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            VStack(spacing: 4) {
                Image(systemName: system).font(.system(size: 30))
                Text(title).font(.system(size: 11, weight: .semibold, design: .monospaced))
            }
            .foregroundStyle(active ? tint : .white.opacity(0.85))
            .frame(width: 92, height: 64)
            .background(active ? tint.opacity(0.18) : .white.opacity(0.06))
            .clipShape(RoundedRectangle(cornerRadius: 12))
        }
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
// range server (AVPlayer). If the hardware refuses the 4:2:2 10-bit HEVC the
// X-H2S records, the plan's fallback is an MPVKit screen — the pull-a-local-copy
// path below also gives AVPlayer a plain file to chew on.
struct VideoFrame: View {
    @ObservedObject var model: GridModel
    let shot: Shot
    let active: Bool

    @State private var player: AVPlayer?
    @State private var failed = false

    var body: some View {
        ZStack {
            Color.black
            if let player {
                VideoPlayer(player: player)
                    .onDisappear { player.pause() }
            } else if failed {
                VStack(spacing: 10) {
                    Image(systemName: "film").font(.system(size: 40)).foregroundStyle(Color.amber)
                    Text("VIDEO UNAVAILABLE").font(.system(.headline, design: .monospaced))
                    Text("pull a local copy to play it")
                        .font(.system(.caption, design: .monospaced)).foregroundStyle(.secondary)
                    Button("PULL VIDEO") { model.loadVideo(shot.id) }
                        .buttonStyle(.borderedProminent).tint(Color.amber)
                }
            } else {
                ProgressView().tint(.white)
            }
        }
        .onChange(of: active) { on in
            if on { start() } else { stop() }
        }
        .onAppear { if active { start() } }
        .onDisappear { stop() }
    }

    private func start() {
        guard player == nil, let url = model.videoURL(shot.id) else { return }
        let item = AVPlayerItem(url: url)
        let p = AVPlayer(playerItem: item)
        p.actionAtItemEnd = .pause
        player = p
        p.play()
        // surface a hard failure so the user can fall back to a local pull
        Task {
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            if item.status == .failed { failed = true; player = nil }
        }
    }

    private func stop() {
        player?.pause()
        player = nil
    }
}

// Filmstrip is the horizontal thumbnail strip under the frame: current one
// highlighted, decisions marked, tap to jump; auto-scrolls to the cursor.
struct Filmstrip: View {
    @ObservedObject var model: GridModel
    @Binding var index: Int

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView(.horizontal, showsIndicators: false) {
                LazyHStack(spacing: 3) {
                    ForEach(Array(model.shots.enumerated()), id: \.element.id) { i, s in
                        let decision = model.decisions[s.id] ?? ""
                        ZStack(alignment: .bottom) {
                            if let url = model.thumbURL(s.id, i) {
                                ThumbView(url: url, cacheKey: "\(s.id):\(model.orientOf(i))", ready: model.thumbReady(i))
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
            .background(Color.black.opacity(0.45))
            .onChange(of: index) { i in withAnimation { proxy.scrollTo(i, anchor: .center) } }
            .onAppear { proxy.scrollTo(index, anchor: .center) }
        }
    }
}

// ZoomableImage loads a full frame from /api/image (the engine demand-fetches
// and blocks until ready) and supports pinch-zoom + double-tap.
struct ZoomableImage: View {
    let url: URL?
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
                                .onEnded { v in scale = min(max(scale * v, 1), 5) }
                        )
                        .onTapGesture(count: 2) { withAnimation { scale = scale > 1 ? 1 : 2.5 } }
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
        image = nil; loadFailed = false; scale = 1
        guard let url,
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let img = UIImage(data: data) else { loadFailed = true; return }
        image = img
    }
}
