import Foundation
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
    private var settings = AppSettings()

    var baseURL: URL? { port > 0 ? URL(string: "http://127.0.0.1:\(port)") : nil }
    var defaultImportDest: String { settings.importDest }

    private var docs: URL { FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0] }

    // MARK: - lifecycle

    func start(_ s: AppSettings) {
        guard engine == nil else { return }
        settings = s
        ICCTransport.shared.start()
        Task { await boot() }
    }

    /// Settings changed (or the link changed): tear the engine down and rebuild.
    func restart(_ s: AppSettings) {
        settings = s
        stop()
        Task { await boot() }
    }

    func stop() {
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

        // Give ImageCaptureCore a moment to report an attached camera before
        // falling back to the fake corpus.
        if !settings.forceFake {
            status = "looking for a camera…"
            for _ in 0..<20 {
                if ICCTransport.shared.connected() { break }
                try? await Task.sleep(nanoseconds: 250_000_000)
            }
        }

        var e: MobileEngine?
        var nsErr: NSError?
        if !settings.forceFake && ICCTransport.shared.connected() {
            status = "starting engine (camera)…"
            e = MobileStartICC(dataDir, cacheDir, ICCTransport.shared,
                               settings.immichURL, settings.immichKey,
                               settings.session, settings.stack, &nsErr)
            mode = .camera
        } else {
            status = "starting engine (fake corpus)…"
            let corpus = docs.appendingPathComponent("fake-corpus").path
            try? FileManager.default.createDirectory(atPath: corpus, withIntermediateDirectories: true)
            var seedErr: NSError?
            MobileSeedFakeCorpus(corpus, 6, 40, &seedErr)
            if let seedErr {
                startError = seedErr.localizedDescription
                status = "corpus seed failed"
                return
            }
            e = MobileStartLocal(dataDir, cacheDir, corpus, settings.session, &nsErr)
            mode = .fake
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
        cameraStatus = ICCTransport.shared.status
        // fold camera-link events into the engine log so the in-app diagnostics
        // screen is the single place to look when debugging wirelessly
        for line in ICCTransport.shared.drainLog() { engine?.logEvent("icc: " + line) }
        guard let e = engine else { return }
        ready = e.ready()
        shotCount = e.shotCount()
        status = ready ? "ready" : e.discoveryStatus()
        log = e.fullLog()

        // Started on the fake corpus but a camera has since been plugged in —
        // switch over (the fake backend is only ever a fallback).
        if mode == .fake && !settings.forceFake && ICCTransport.shared.connected() {
            logEvent("camera attached — restarting engine on the ImageCaptureCore link")
            restart(settings)
        }
    }

    // MARK: - passthroughs

    func nudge() { engine?.nudge() }
    func logEvent(_ msg: String) { engine?.logEvent(msg) }
    func fullLog() -> String { engine?.fullLog() ?? "engine not running" }

    deinit { pollTask?.cancel() }
}
