import Foundation
import AVFAudio
import Mobile

// Engine wraps the gomobile fuji-cull core and decides which camera link to run:
//
//   • camera — StartICC over ICCTransport (ImageCaptureCore), the real iPad path
//   • fake   — StartLocal over a synthetic corpus, so the simulator (and a device
//              with nothing plugged in) still exercises the whole app
//
// Everything above this talks to the engine over the loopback HTTP API.
@MainActor
final class Engine: ObservableObject {
    enum Mode: String { case none, camera, fake }

    @Published var mode: Mode = .none
    @Published var status: String = "starting engine…"
    @Published var cameraStatus: String = ""
    @Published var ready: Bool = false
    @Published var shotCount: Int = 0
    @Published var port: Int = 0
    @Published var log: String = ""
    @Published var startError: String? = nil
    /// Bumps on every (re)start so views rebuild — mirrors Android's `epoch`.
    @Published var epoch: Int = 0

    private var engine: MobileEngine?
    private var pollTask: Task<Void, Never>?
    private var bootTask: Task<Void, Never>?
    private var settings = AppSettings()

    /// The fake corpus exists so the app is buildable/testable without a camera.
    /// On real hardware it is never entered automatically — culling a synthetic
    /// corpus while believing you are looking at the card would be far worse
    /// than waiting for the camera. Only an explicit Settings opt-in allows it.
    private var fakeAllowed: Bool {
        #if targetEnvironment(simulator)
        return true
        #else
        return settings.forceFake
        #endif
    }

    var baseURL: URL? { port > 0 ? URL(string: "http://127.0.0.1:\(port)") : nil }
    var defaultImportDest: String { settings.importDest }

    private var docs: URL { FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0] }

    // MARK: - lifecycle

    func start(_ s: AppSettings) {
        guard engine == nil, bootTask == nil else { return }
        settings = s
        // A real playback session: the default (.soloAmbient) is the most
        // interruptible category iOS has, and interruptions pause AVPlayer
        // silently — probe-measured as videos freezing seconds in with a
        // full buffer and rate=0.
        try? AVAudioSession.sharedInstance().setCategory(.playback, mode: .moviePlayback)
        ICCTransport.shared.start()
        bootTask = Task { await boot() }
    }

    /// Settings changed (or the link changed): tear the engine down and rebuild.
    func restart(_ s: AppSettings) {
        settings = s
        stop()
        bootTask = Task { await boot() }
    }

    func stop() {
        bootTask?.cancel(); bootTask = nil
        pollTask?.cancel(); pollTask = nil
        engine?.stop()
        engine = nil
        ready = false
        shotCount = 0
        port = 0
        mode = .none
        status = "restarting engine…"
    }

    private func boot() async {
        startError = nil
        let dataDir = docs.appendingPathComponent("engine").path
        let cacheDir = docs.appendingPathComponent("cache").path
        for p in [dataDir, cacheDir] {
            try? FileManager.default.createDirectory(atPath: p, withIntermediateDirectories: true)
        }

        // Cross-device sync config reaches the Go engine via env (set before any
        // MobileStart*), avoiding gomobile signature changes.
        MobileSetEnv("FUJI_SYNC_URL", settings.syncURL.trimmingCharacters(in: .whitespaces))
        MobileSetEnv("FUJI_SYNC_KEY", settings.syncKey.trimmingCharacters(in: .whitespaces))

        var e: MobileEngine?
        var nsErr: NSError?

        if settings.forceFake {
            // explicit opt-in only
            guard let fake = startFake(dataDir: dataDir, cacheDir: cacheDir, err: &nsErr) else { return }
            e = fake
            mode = .fake
        } else {
            // Wait for a camera session — connected() flips as soon as the
            // session opens, so the engine starts within seconds and its
            // discover loop retries PTP sweeps through ICC's ~150s startup
            // block (progress shows in the app). On real hardware we wait
            // indefinitely. Only the simulator (where a camera can never
            // appear) falls through to the fake corpus, after a short grace
            // period. The link log is NOT drained here: it accumulates in the
            // transport and flushes into the engine log once the engine is up.
            var waited = 0
            while !ICCTransport.shared.connected() {
                if Task.isCancelled { return }
                status = "waiting for the camera…"
                cameraStatus = ICCTransport.shared.status
                #if targetEnvironment(simulator)
                if waited >= 8 { break }   // ~2s: no camera is ever coming here
                #endif
                try? await Task.sleep(nanoseconds: 250_000_000)
                waited += 1
            }

            if ICCTransport.shared.connected() {
                status = "starting engine (camera)…"
                e = MobileStartICC(dataDir, cacheDir, ICCTransport.shared,
                                   settings.immichURL, settings.immichKey,
                                   "", settings.stack, &nsErr)
                mode = .camera
            } else if fakeAllowed {
                guard let fake = startFake(dataDir: dataDir, cacheDir: cacheDir, err: &nsErr) else { return }
                e = fake
                mode = .fake
            } else {
                status = "no camera"
                return
            }
        }

        guard let e else {
            startError = nsErr?.localizedDescription ?? "engine failed to start"
            status = "start failed"
            mode = .none
            return
        }
        engine = e
        port = e.port()
        epoch += 1
        startPolling()
    }

    /// Seeds and starts the synthetic corpus (simulator, or explicit opt-in).
    private func startFake(dataDir: String, cacheDir: String, err: inout NSError?) -> MobileEngine? {
        status = "starting engine (fake corpus)…"
        let corpus = docs.appendingPathComponent("fake-corpus").path
        try? FileManager.default.createDirectory(atPath: corpus, withIntermediateDirectories: true)
        var seedErr: NSError?
        MobileSeedFakeCorpus(corpus, 6, 40, &seedErr)
        if let seedErr {
            startError = seedErr.localizedDescription
            status = "corpus seed failed"
            return nil
        }
        return MobileStartLocal(dataDir, cacheDir, corpus, "", &err)
    }

    // MARK: - polling

    private func startPolling() {
        pollTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                self?.poll()
                try? await Task.sleep(nanoseconds: 600_000_000)
            }
        }
    }

    private func poll() {
        // breadcrumbed: main deadlocks permanently mid-session; a call that
        // never returns can't log its own duration, so each is bracketed with
        // direct-to-disk breadcrumbs — the unclosed "→" names the blocker
        func timed<T>(_ name: String, _ f: () -> T) -> T {
            DebugProbe.trace("→poll.\(name)")
            let t0 = Date()
            let v = f()
            let ms = Int(Date().timeIntervalSince(t0) * 1000)
            DebugProbe.trace("←poll.\(name) \(ms)ms")
            if ms > 500 { NSLog("[hang] poll.%@ took %dms", name, ms); engine?.logEvent("hang: poll.\(name) \(ms)ms") }
            return v
        }
        // Every @Published assignment fires objectWillChange and re-renders
        // every observer of the engine — at 24k shots an unconditional set
        // each 600ms kept SwiftUI in a render pass longer than the poll
        // period, permanently starving the main thread (probe-verified: the
        // hung stack was AttributeGraph churn, not a lock). So: assign only
        // on change, and keep the connect-screen log tail SMALL — the full
        // 3000-line string re-laid-out per poll was the biggest single input.
        let cs = timed("status") { ICCTransport.shared.status }
        if cs != cameraStatus { cameraStatus = cs }
        // fold camera-link events into the engine log so the in-app diagnostics
        // screen is the single place to look when debugging wirelessly
        for line in timed("drainLog", { ICCTransport.shared.drainLog() }) { engine?.logEvent("icc: " + line) }
        guard let e = engine else { return }
        let r = timed("ready") { e.ready() }
        if r != ready { ready = r }
        let sc = timed("shotCount") { e.shotCount() }
        if sc != shotCount { shotCount = sc }
        let st = r ? "ready" : timed("discoveryStatus") { e.discoveryStatus() }
        if st != status { status = st }
        if !r {
            // connect screen only; the grid never renders this string and the
            // log sheet fetches its own copy on demand
            let tail = Self.lastLines(timed("fullLog") { e.fullLog() }, 30)
            if tail != log { log = tail }
        }
    }

    private static func lastLines(_ s: String, _ n: Int) -> String {
        var lines = [Substring]()
        var end = s.endIndex
        while lines.count < n, let nl = s[..<end].lastIndex(of: "\n") {
            lines.append(s[s.index(after: nl)..<end])
            end = nl
        }
        if lines.count < n { lines.append(s[..<end]) }
        return lines.reversed().joined(separator: "\n")
    }

    // MARK: - passthroughs

    func nudge() { engine?.nudge() }
    func logEvent(_ msg: String) { engine?.logEvent(msg) }
    func fullLog() -> String { engine?.fullLog() ?? ICCTransport.shared.recentLog() }

    deinit { pollTask?.cancel() }
}
