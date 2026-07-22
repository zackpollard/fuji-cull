import SwiftUI

// GridModel holds the catalog + decisions + thumbnail states, polling the
// engine so tiles fill in as the background sweep produces thumbnails and
// decisions/counts stay live. Talks to the engine over the loopback API.
@MainActor
final class GridModel: ObservableObject {
    @Published var shots: [Shot] = []
    @Published var decisions: [String: String] = [:]
    @Published var counts: [String: Int] = [:]
    @Published var cursor: Int = 0
    @Published private(set) var haveThumbs: Int = 0
    @Published var showViewer = false
    @Published var viewerIndex = 0
    @Published var importStatus: ImportStatus?

    private var states: [Character] = []
    private var orientChars: [Character] = []
    private var api: API?
    private var pollTask: Task<Void, Never>?

    func attach(base: URL?) {
        guard api == nil, let base else { return }
        api = API(base: base)
    }

    func load() async {
        guard let api, let st = try? await api.fetchState() else { return }
        shots = st.shots
        decisions = st.decisions
        counts = st.counts
        cursor = st.cursor
    }

    func startPolling() {
        guard pollTask == nil, let api else { return }
        pollTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                if let t = try? await api.fetchThumbs() {
                    self?.states = Array(t.states)
                    self?.orientChars = Array(t.orient)
                    self?.haveThumbs = t.have
                }
                if let st = try? await api.fetchState() {
                    self?.decisions = st.decisions
                    self?.counts = st.counts
                    self?.importStatus = st.importStatus
                }
                try? await Task.sleep(nanoseconds: 600_000_000)
            }
        }
    }

    func thumbReady(_ i: Int) -> Bool { i < states.count && states[i] == "1" }
    func orientOf(_ i: Int) -> Int { i < orientChars.count ? Int(String(orientChars[i])) ?? 0 : 0 }

    func thumbURL(_ id: String, _ i: Int) -> URL? { api?.thumbURL(id, orient: orientOf(i)) }
    func imageURL(_ id: String) -> URL? { api?.imageURL(id) }

    func select(_ i: Int) {
        cursor = i
        Task { await api?.setCursor(i); await api?.setThumbHint(i) }
    }

    func openViewer(_ i: Int) {
        select(i)
        viewerIndex = i
        showViewer = true
    }

    // Toggle a decision (clears if already set) — grid quick-triage.
    func decide(_ shot: Shot, _ d: String) {
        let current = decisions[shot.id] ?? ""
        let next = current == d ? "clear" : d
        decisions[shot.id] = next == "clear" ? "" : next
        Task { await api?.decide(shot.id, next) }
    }

    // Set a decision outright (viewer keep/reject buttons).
    func setDecision(_ shot: Shot, _ d: String) {
        decisions[shot.id] = d
        Task { await api?.decide(shot.id, d) }
    }

    func startImport(dest: String, album: String) {
        Task { await api?.startImport(dest: dest, album: album) }
    }

    deinit { pollTask?.cancel() }
}

struct GridView: View {
    @EnvironmentObject var engine: Engine
    @StateObject private var model = GridModel()
    @State private var showLog = false
    @State private var showImport = false
    @State private var showSettings = false

    private let columns = [GridItem(.adaptive(minimum: 120, maximum: 180), spacing: 3)]

    var body: some View {
        VStack(spacing: 0) {
            HeaderBar(model: model,
                      onImport: { showImport = true },
                      onLog: { showLog = true },
                      onSettings: { showSettings = true })
            ScrollView {
                LazyVGrid(columns: columns, spacing: 3) {
                    ForEach(Array(model.shots.enumerated()), id: \.element.id) { i, shot in
                        Tile(shot: shot, index: i, model: model)
                    }
                }
                .padding(3)
            }
        }
        .background(Color(red: 0.043, green: 0.047, blue: 0.043).ignoresSafeArea())
        .task {
            model.attach(base: engine.baseURL)
            await model.load()
            model.startPolling()
            // test hooks: `-autoViewer N` opens the viewer at frame N; `-autoImport 1`
            // opens the import sheet — for non-interactive screenshots.
            let av = UserDefaults.standard.integer(forKey: "autoViewer")
            if av > 0 && av <= model.shots.count { model.openViewer(av - 1) }
            if UserDefaults.standard.bool(forKey: "autoImport") { showImport = true }
        }
        .fullScreenCover(isPresented: $model.showViewer) {
            ViewerView(model: model, index: $model.viewerIndex)
        }
        .sheet(isPresented: $showLog) {
            LogSheet(text: engine.log, port: engine.port)
        }
        .sheet(isPresented: $showImport) {
            ImportView(model: model, defaultDest: engine.defaultImportDest)
        }
        .sheet(isPresented: $showSettings) {
            SettingsView(model: model, engine: engine)
        }
    }
}

struct HeaderBar: View {
    @ObservedObject var model: GridModel
    var onImport: () -> Void
    var onLog: () -> Void
    var onSettings: () -> Void
    var body: some View {
        HStack(spacing: 14) {
            Text("fuji-cull")
                .foregroundStyle(Color(red: 1.0, green: 0.70, blue: 0.18))
            Spacer()
            Text("TH \(model.haveThumbs)/\(model.shots.count)").foregroundStyle(.tertiary)
            Text("K \(model.counts["keep"] ?? 0)").foregroundStyle(Color(red: 0.22, green: 0.84, blue: 0.48))
            Text("X \(model.counts["reject"] ?? 0)").foregroundStyle(Color(red: 1.0, green: 0.35, blue: 0.24))
            Menu {
                Button { onImport() } label: { Label("Import keepers", systemImage: "square.and.arrow.down") }
                Button { onLog() } label: { Label("Diagnostics", systemImage: "terminal") }
                Button { onSettings() } label: { Label("Settings", systemImage: "gearshape") }
            } label: {
                Image(systemName: "ellipsis.circle")
            }
            .foregroundStyle(.secondary)
        }
        .font(.system(.caption, design: .monospaced))
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(Color.black.opacity(0.55))
    }
}

// LogSheet shows the engine diagnostics log (the connect screen's tail, full).
struct LogSheet: View {
    let text: String
    let port: Int
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        NavigationStack {
            LogTailView(text: text)
                .padding(8)
                .navigationTitle("engine :\(port)")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Done") { dismiss() }
                    }
                }
        }
    }
}

struct Tile: View {
    let shot: Shot
    let index: Int
    @ObservedObject var model: GridModel

    var body: some View {
        let decision = model.decisions[shot.id] ?? ""
        ZStack(alignment: .bottom) {
            if let url = model.thumbURL(shot.id, index) {
                ThumbView(url: url, cacheKey: "\(shot.id):\(model.orientOf(index))", ready: model.thumbReady(index))
            } else {
                Rectangle().fill(Color.white.opacity(0.04))
            }
            if decision == "keep" || decision == "reject" {
                Rectangle()
                    .fill(decision == "keep" ? Color(red: 0.22, green: 0.84, blue: 0.48) : Color(red: 1.0, green: 0.35, blue: 0.24))
                    .frame(height: 5)
            }
        }
        .aspectRatio(3.0 / 2.0, contentMode: .fill)
        .clipped()
        .overlay(alignment: .topTrailing) {
            if shot.kind == "video" {
                Image(systemName: "play.circle.fill")
                    .foregroundStyle(.white)
                    .padding(5)
                    .shadow(radius: 2)
            }
        }
        .overlay(
            Rectangle().stroke(model.cursor == index ? Color(red: 1.0, green: 0.70, blue: 0.18) : .clear, lineWidth: 2)
        )
        .contentShape(Rectangle())
        .onTapGesture { model.openViewer(index) }
    }
}
