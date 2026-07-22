import SwiftUI

// Immich-style timeline grouping: month sections containing day groups, with
// each shot keeping its catalog index (thumb/orient/immich state strings are
// indexed by shot, not by row).
struct DayGroup: Identifiable {
    let id: String
    let label: String
    let cells: [(index: Int, shot: Shot)]
}

struct MonthGroup: Identifiable {
    let id: String
    let label: String
    let days: [DayGroup]
}

@MainActor
final class GridModel: ObservableObject {
    @Published var shots: [Shot] = []
    @Published var groups: [MonthGroup] = []
    @Published var decisions: [String: String] = [:]
    @Published var counts: [String: Int] = [:]
    @Published var cursor: Int = 0
    @Published private(set) var haveThumbs: Int = 0
    @Published private(set) var exifKnown: Int = 0
    @Published private(set) var exifTotal: Int = 0
    @Published var sick = false
    @Published var enginePosters = true
    @Published var importing = ""
    @Published var importStatus: ImportStatus?
    @Published var fetchStates: [String: String] = [:]
    @Published var showViewer = false
    @Published var viewerIndex = 0

    private var states: [Character] = []
    private var orientChars: [Character] = []
    private var immichChars: [Character] = []
    private var api: API?
    private var pollTask: Task<Void, Never>?

    func attach(base: URL?) {
        guard api == nil, let base else { return }
        api = API(base: base)
    }

    func load() async {
        guard let api else { return }
        // the catalog can lag readiness for a beat; retry until it lands
        while shots.isEmpty {
            if let st = try? await api.fetchState(), !st.shots.isEmpty {
                shots = st.shots
                groups = Self.group(st.shots)
                decisions = st.decisions
                counts = st.counts
                cursor = st.cursor
                break
            }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
    }

    func startPolling() {
        guard pollTask == nil, let api else { return }
        pollTask = Task { @MainActor [weak self] in
            var lastHave = -1
            while !Task.isCancelled {
                if let t = try? await api.fetchThumbs() {
                    self?.states = Array(t.states)
                    self?.orientChars = Array(t.orient)
                    self?.immichChars = Array(t.immich)
                    self?.haveThumbs = t.have
                    self?.exifKnown = t.orient.filter { $0 >= "1" && $0 <= "8" }.count
                    self?.exifTotal = t.orient.filter { $0 != "-" }.count
                }
                if let st = try? await api.fetchStatus() {
                    self?.sick = st.bulkSick || st.partSick
                    self?.enginePosters = st.posters
                    self?.decisions = st.decisions
                    self?.counts = st.counts
                    self?.fetchStates = st.fetch
                    self?.importStatus = st.importStatus
                    if let imp = st.importStatus {
                        if imp.running {
                            self?.importing = "importing \(imp.done)/\(imp.total)"
                        } else if let cur = self?.importing, !cur.isEmpty, cur != "import done" {
                            self?.importing = imp.error.isEmpty ? "import done" : imp.error
                        }
                    }
                }
                // poll fast while thumbnails are still landing, then back off
                let have = self?.haveThumbs ?? 0
                let populating = have != lastHave
                lastHave = have
                try? await Task.sleep(nanoseconds: populating ? 600_000_000 : 2_000_000_000)
            }
        }
    }

    // MARK: - per-shot state

    func thumbReady(_ i: Int) -> Bool { i < states.count && states[i] == "1" }
    func orientOf(_ i: Int) -> Int { i < orientChars.count ? Int(String(orientChars[i])) ?? 0 : 0 }
    func inImmich(_ i: Int) -> Bool { i < immichChars.count && immichChars[i] == "1" }
    func buffered(_ id: String) -> Bool { fetchStates[id] == "ready" }
    func failed(_ id: String) -> Bool { fetchStates[id] == "failed" }

    func thumbURL(_ id: String, _ i: Int) -> URL? { api?.thumbURL(id, orient: orientOf(i)) }
    func imageURL(_ id: String) -> URL? { api?.imageURL(id) }
    func videoURL(_ id: String) -> URL? { api?.videoURL(id) }

    // MARK: - actions

    func select(_ i: Int) {
        cursor = i
        Task { await api?.setCursor(i) }
    }
    func hint(_ i: Int) { Task { await api?.setThumbHint(i) } }

    func openViewer(_ i: Int) {
        select(i)
        viewerIndex = i
        showViewer = true
    }

    func decide(_ shot: Shot, _ d: String) {
        let current = decisions[shot.id] ?? ""
        let next = current == d ? "" : d
        decisions[shot.id] = next
        Task { await api?.decide(shot.id, next) }
    }

    func setDecision(_ shot: Shot, _ d: String) {
        decisions[shot.id] = d
        Task { await api?.decide(shot.id, d) }
    }

    func retry(_ id: String) { Task { await api?.retryShot(id) } }
    func loadVideo(_ id: String) { Task { await api?.loadVideo(id) } }
    func startImport(dest: String, album: String) {
        importing = "importing…"
        Task { await api?.startImport(dest: dest, album: album) }
    }

    deinit { pollTask?.cancel() }

    // MARK: - grouping

    static func group(_ shots: [Shot]) -> [MonthGroup] {
        var months: [MonthGroup] = []
        var curMonth = "\u{0}", curDay = "\u{0}"
        var monthDays: [DayGroup] = []
        var dayCells: [(Int, Shot)] = []
        var monthLabel = "", dayLabel = "", monthID = "", dayID = ""

        func flushDay() {
            guard !dayCells.isEmpty else { return }
            monthDays.append(DayGroup(id: dayID, label: dayLabel,
                                      cells: dayCells.map { (index: $0.0, shot: $0.1) }))
            dayCells = []
        }
        func flushMonth() {
            flushDay()
            guard !monthDays.isEmpty else { return }
            months.append(MonthGroup(id: monthID, label: monthLabel, days: monthDays))
            monthDays = []
        }

        for (i, s) in shots.enumerated() {
            let date = s.date ?? ""
            let day = date.isEmpty ? s.folder : date
            let month = date.count >= 7 ? String(date.prefix(7)) : day
            if month != curMonth {
                flushMonth()
                curMonth = month; curDay = "\u{0}"
                monthID = month; monthLabel = prettyMonth(month)
            }
            if day != curDay {
                flushDay()
                curDay = day
                dayID = "\(month)/\(day)"; dayLabel = prettyDay(day)
            }
            dayCells.append((i, s))
        }
        flushMonth()
        return months
    }

    private static let monthIn: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "yyyy-MM"; return f
    }()
    private static let dayIn: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "yyyy-MM-dd"; return f
    }()

    static func prettyMonth(_ key: String) -> String {
        guard let d = monthIn.date(from: key) else { return key }
        let f = DateFormatter(); f.dateFormat = "MMMM yyyy"
        return f.string(from: d)
    }
    static func prettyDay(_ key: String) -> String {
        guard let d = dayIn.date(from: key) else { return key }
        let f = DateFormatter(); f.dateFormat = "EEE d MMM yyyy"
        return f.string(from: d)
    }
}

struct GridView: View {
    @EnvironmentObject var engine: Engine
    @EnvironmentObject var store: SettingsStore
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
            if model.shots.isEmpty {
                Spacer()
                ProgressView("loading catalog…").tint(Color.amber)
                Spacer()
            } else {
                timeline
            }
        }
        .background(Color.appBG.ignoresSafeArea())
        .task {
            model.attach(base: engine.baseURL)
            await model.load()
            model.startPolling()
            let av = UserDefaults.standard.integer(forKey: "autoViewer")
            if av > 0 && av <= model.shots.count { model.openViewer(av - 1) }
            if UserDefaults.standard.bool(forKey: "autoImport") { showImport = true }
        }
        .fullScreenCover(isPresented: $model.showViewer) {
            ViewerView(model: model, index: $model.viewerIndex)
        }
        .sheet(isPresented: $showLog) { LogSheet(engine: engine) }
        .sheet(isPresented: $showImport) {
            ImportView(model: model, defaultDest: store.settings.importDest, album: store.settings.album)
        }
        .sheet(isPresented: $showSettings) { SettingsView() }
    }

    private var timeline: some View {
        ScrollViewReader { proxy in
            HStack(spacing: 0) {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                        ForEach(model.groups) { month in
                            Section {
                                ForEach(month.days) { day in
                                    Text(day.label)
                                        .font(.system(.caption, design: .monospaced))
                                        .foregroundStyle(.secondary)
                                        .padding(.horizontal, 8).padding(.top, 10).padding(.bottom, 4)
                                    LazyVGrid(columns: columns, spacing: 3) {
                                        ForEach(day.cells, id: \.shot.id) { cell in
                                            Tile(shot: cell.shot, index: cell.index, model: model)
                                                .onAppear { model.hint(cell.index) }
                                        }
                                    }
                                    .padding(.horizontal, 3)
                                }
                            } header: {
                                MonthHeader(label: month.label)
                            }
                            .id(month.id)
                        }
                    }
                }
                MonthScrubber(months: model.groups) { id in
                    withAnimation { proxy.scrollTo(id, anchor: .top) }
                }
            }
        }
    }
}

struct MonthHeader: View {
    let label: String
    var body: some View {
        Text(label)
            .font(.system(.subheadline, design: .monospaced).weight(.semibold))
            .foregroundStyle(Color.amber)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 8).padding(.vertical, 6)
            .background(Color.appBG.opacity(0.95))
    }
}

// MonthScrubber is the right-edge jump strip — the touch equivalent of the
// desktop timeline scrubber, for crossing thousands of shots quickly.
struct MonthScrubber: View {
    let months: [MonthGroup]
    let onJump: (String) -> Void

    var body: some View {
        VStack(spacing: 2) {
            ForEach(months) { m in
                Text(shortLabel(m.label))
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .frame(maxHeight: .infinity)
                    .contentShape(Rectangle())
                    .onTapGesture { onJump(m.id) }
            }
        }
        .frame(width: 46)
        .padding(.vertical, 6)
        .background(Color.black.opacity(0.25))
    }

    private func shortLabel(_ l: String) -> String {
        let parts = l.split(separator: " ")
        guard parts.count == 2 else { return l }
        return "\(parts[0].prefix(3))\n\(parts[1].suffix(2))"
    }
}

struct HeaderBar: View {
    @ObservedObject var model: GridModel
    var onImport: () -> Void
    var onLog: () -> Void
    var onSettings: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Button(action: onLog) {
                VStack(alignment: .leading, spacing: 1) {
                    Text("K \(model.counts["keep"] ?? 0)  X \(model.counts["reject"] ?? 0)  · \(model.counts["undecided"] ?? 0)")
                        .foregroundStyle(.white)
                    Text("th \(model.haveThumbs)/\(model.shots.count) · ex \(model.exifKnown)/\(model.exifTotal)"
                         + (model.sick ? " · CAMERA SICK" : ""))
                        .foregroundStyle(model.sick ? Color.rejectRed : .secondary)
                        .font(.system(size: 10, design: .monospaced))
                }
            }
            .buttonStyle(.plain)

            Spacer()
            if !model.importing.isEmpty {
                Text(model.importing).foregroundStyle(Color.amber)
            }
            Button(action: onSettings) { Image(systemName: "gearshape") }
                .foregroundStyle(.secondary)
            Button(action: onImport) { Text("IMPORT").bold() }
                .buttonStyle(.borderedProminent)
                .tint(Color.amber)
        }
        .font(.system(.caption, design: .monospaced))
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(Color.black.opacity(0.55))
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
                    .fill(decision == "keep" ? Color.keepGreen : Color.rejectRed)
                    .frame(height: 5)
            }
        }
        .aspectRatio(3.0 / 2.0, contentMode: .fill)
        .clipped()
        .overlay(alignment: .topLeading) {
            if model.inImmich(index) {
                Image(systemName: "cloud.fill")
                    .font(.system(size: 10))
                    .foregroundStyle(.white.opacity(0.9))
                    .padding(4)
                    .shadow(radius: 2)
            }
        }
        .overlay(alignment: .topTrailing) {
            HStack(spacing: 3) {
                if model.buffered(shot.id) {
                    Circle().fill(Color(red: 0.18, green: 0.5, blue: 0.88)).frame(width: 6, height: 6)
                }
                if shot.kind == "video" {
                    Image(systemName: "play.circle.fill").foregroundStyle(.white).shadow(radius: 2)
                }
            }
            .padding(4)
        }
        .overlay(
            Rectangle().stroke(model.cursor == index ? Color.amber : .clear, lineWidth: 2)
        )
        .contentShape(Rectangle())
        .onTapGesture { model.openViewer(index) }
    }
}
