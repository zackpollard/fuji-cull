import SwiftUI
import AVFoundation

// Immich-style timeline grouping: month sections containing day groups, with
// each shot keeping its catalog index (thumb/orient/immich state strings are
// indexed by shot, not by row).
struct DayGroup: Identifiable {
    let id: String
    let label: String
    // catalog indices, NOT (index, shot) tuples: ForEach re-diffs this array
    // on every render pass, and copying tuple-of-struct arrays (retain/release
    // per string field, ~10k elements for a big day) kept passes longer than
    // the poll period at 24k shots — the probe's hung stacks were exactly
    // swift_arrayInitWithCopy/arrayDestroy. [Int] diffs as a memcmp.
    let cells: [Int]
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
    // Videos count toward the engine's thumb total but it can never satisfy
    // them on mobile (no ffmpeg), so their posters — made client-side — are
    // added to the header count; otherwise it sits short forever and reads
    // like a stall.
    @Published private(set) var posterCount: Int = 0
    @Published private(set) var exifKnown: Int = 0
    @Published private(set) var exifTotal: Int = 0
    @Published var sick = false
    @Published var enginePosters = true
    @Published var importing = ""
    @Published var importStatus: ImportStatus?
    @Published var fetchStates: [String: String] = [:]
    @Published var showViewer = false
    @Published var viewerIndex = 0

    @Published private(set) var states: [Character] = []
    @Published private(set) var orientChars: [Character] = []
    @Published private(set) var immichChars: [Character] = []
    private var api: API?
    private var pollTask: Task<Void, Never>?
    private var posterTask: Task<Void, Never>?

    func attach(base: URL?) {
        guard api == nil, let base else { return }
        api = API(base: base)
    }

    func load() async {
        guard let api else { return }
        // the catalog can lag readiness for a beat; retry until it lands
        while shots.isEmpty {
            if let st = try? await api.fetchState(), !st.shots.isEmpty {
                // group off the main actor: at 24k shots this pass plus the
                // publish froze first render for ~3s when run inline
                let grouped = await Task.detached(priority: .userInitiated) {
                    Self.group(st.shots)
                }.value
                shots = st.shots
                groups = grouped
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
            // change-guard every @Published assignment: each one invalidates
            // every observer, and at 24k shots gratuitous invalidations from
            // this loop kept SwiftUI re-rendering longer than the poll period
            var rawStates = "", rawOrient = "", rawImmich = ""
            while !Task.isCancelled {
                if let t = try? await api.fetchThumbs(), let self {
                    if t.states != rawStates { rawStates = t.states; self.states = Array(t.states) }
                    if t.orient != rawOrient {
                        rawOrient = t.orient
                        self.orientChars = Array(t.orient)
                        self.exifKnown = t.orient.filter { $0 >= "1" && $0 <= "8" }.count
                        self.exifTotal = t.orient.filter { $0 != "-" }.count
                    }
                    if t.immich != rawImmich { rawImmich = t.immich; self.immichChars = Array(t.immich) }
                    if t.have != self.haveThumbs { self.haveThumbs = t.have }
                }
                if let st = try? await api.fetchStatus(), let self {
                    let sick = st.bulkSick || st.partSick
                    if sick != self.sick { self.sick = sick }
                    if st.posters != self.enginePosters { self.enginePosters = st.posters }
                    if st.decisions != self.decisions { self.decisions = st.decisions }
                    if st.counts != self.counts { self.counts = st.counts }
                    if st.fetch != self.fetchStates { self.fetchStates = st.fetch }
                    if st.importStatus != self.importStatus { self.importStatus = st.importStatus }
                    self.startPosterSweepIfNeeded()
                    if let imp = st.importStatus {
                        if imp.running {
                            self.importing = "importing \(imp.done)/\(imp.total)"
                        } else if !self.importing.isEmpty, self.importing != "import done" {
                            self.importing = imp.error.isEmpty ? "import done" : imp.error
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
    /// Fold a client-side event into the engine log — the one place the
    /// in-app diagnostics and the pulled engine.log both show.
    func logEvent(_ msg: String) { Task { await api?.logEvent(msg) } }
    func startImport(dest: String, album: String) {
        importing = "importing…"
        Task { await api?.startImport(dest: dest, album: album) }
    }

    deinit { pollTask?.cancel(); posterTask?.cancel() }

    /// Client-side video posters, mirroring the Android build: the engine has
    /// no ffmpeg on mobile (status reports posters=false), so once the
    /// catalog is up a single serial sweep pulls each video's 8 MB head and
    /// extracts frame 0 locally (Posters). Transiently-deferred videos (busy
    /// camera link) are retried each pass; the loop ends when every video has
    /// a poster or a permanent failure marker.
    func startPosterSweepIfNeeded() {
        guard posterTask == nil, let api, !enginePosters, !shots.isEmpty else { return }
        let videos = shots.filter { $0.kind == "video" }
        guard !videos.isEmpty else { return }
        posterTask = Task.detached(priority: .utility) { [weak self] in
            // count what previous runs already cached
            let already = videos.filter { Posters.shared.cached($0) != nil }.count
            await MainActor.run { self?.posterCount = already }
            while !Task.isCancelled {
                var pending = 0
                for shot in videos {
                    if Task.isCancelled { return }
                    if Posters.shared.resolved(shot) { continue }
                    if await Posters.shared.load(api: api, shot: shot) != nil {
                        await MainActor.run { self?.posterCount += 1 }
                    } else if !Posters.shared.isFailed(shot) {
                        pending += 1
                    }
                }
                if pending == 0 { return }
                try? await Task.sleep(nanoseconds: 30_000_000_000)
            }
        }
    }

    // MARK: - grouping

    nonisolated static func group(_ shots: [Shot]) -> [MonthGroup] {
        var months: [MonthGroup] = []
        var curMonth = "\u{0}", curDay = "\u{0}"
        var monthDays: [DayGroup] = []
        var dayCells: [Int] = []
        var monthLabel = "", dayLabel = "", monthID = "", dayID = ""

        func flushDay() {
            guard !dayCells.isEmpty else { return }
            monthDays.append(DayGroup(id: dayID, label: dayLabel, cells: dayCells))
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
            dayCells.append(i)
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

    nonisolated static func prettyMonth(_ key: String) -> String {
        guard let d = monthIn.date(from: key) else { return key }
        let f = DateFormatter(); f.dateFormat = "MMMM yyyy"
        return f.string(from: d)
    }
    nonisolated static func prettyDay(_ key: String) -> String {
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
    @StateObject private var scrub = ScrubState()

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
            if DebugProbe.codecAuditRequested || DebugProbe.openVideoRequested {
                let videoIdx = model.shots.indices.filter { model.shots[$0].kind == "video" }
                let m = model
                Task {
                    // codec audit: profile_idc from hvcC byte 1 & 0x1F —
                    // 1 Main (4:2:0 8-bit), 2 Main10 (4:2:0 10-bit),
                    // 4 Rext (4:2:2). moov-only loads, no playback.
                    var byProfile: [Int: [Int]] = [:] // profile -> shot indices
                    if DebugProbe.codecAuditRequested {
                        for idx in videoIdx {
                            guard let url = m.videoURL(m.shots[idx].id) else { continue }
                            let asset = AVURLAsset(url: url)
                            var profile = -1
                            if let track = try? await asset.loadTracks(withMediaType: .video).first,
                               let fd = try? await track.load(.formatDescriptions).first,
                               let atoms = CMFormatDescriptionGetExtension(
                                   fd, extensionKey: kCMFormatDescriptionExtension_SampleDescriptionExtensionAtoms)
                                   as? [String: Any],
                               let hvcc = atoms["hvcC"] as? Data, hvcc.count > 1 {
                                profile = Int(hvcc[1] & 0x1F)
                            }
                            byProfile[profile, default: []].append(idx)
                        }
                        m.logEvent("codecAudit done: \(videoIdx.count) clips, profiles "
                                   + byProfile.map { "\($0.key): \($0.value.count)" }.sorted().joined(separator: ", "))
                    }
                    guard DebugProbe.openVideoRequested, !videoIdx.isEmpty else { return }
                    if DebugProbe.waitForCrawlRequested {
                        while !ICCTransport.shared.isCrawlComplete {
                            try? await Task.sleep(nanoseconds: 3_000_000_000)
                        }
                        m.logEvent("probe: crawl complete — camera idle, starting playback")
                        try? await Task.sleep(nanoseconds: 3_000_000_000)
                    }
                    // with audit data: play a couple of each profile so both
                    // 4:2:0 and 4:2:2 get an explicit hardware verdict;
                    // without: an even spread
                    var sample: [(String, Int)] = []
                    if byProfile.isEmpty {
                        let step = max(1, videoIdx.count / 6)
                        sample = stride(from: 0, to: videoIdx.count, by: step).prefix(6).map { ("?", videoIdx[$0]) }
                    } else {
                        for (profile, idxs) in byProfile.sorted(by: { $0.key < $1.key }) {
                            for idx in idxs.prefix(2) { sample.append(("p\(profile)", idx)) }
                        }
                    }
                    guard let first = sample.first else { return }
                    m.openViewer(first.1)
                    for (n, pick) in sample.enumerated() {
                        DebugProbe.trace("openVideo \(n + 1)/\(sample.count) [\(pick.0)] -> \(m.shots[pick.1].base)")
                        m.logEvent("probe: playing [\(pick.0)] \(m.shots[pick.1].base)")
                        m.viewerIndex = pick.1
                        try? await Task.sleep(nanoseconds: 18_000_000_000)
                    }
                    m.showViewer = false
                }
            }
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
        // scrubber overlays the grid's right edge, Android-style — it fades
        // in while scrolling instead of reserving a permanent column
        ZStack(alignment: .trailing) {
            TimelineCollection(model: model, scrub: scrub)
            TimelineScrubber(scrub: scrub)
        }
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
                    Text("th \(min(model.haveThumbs + model.posterCount, model.shots.count))/\(model.shots.count) · ex \(model.exifKnown)/\(model.exifTotal)"
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

