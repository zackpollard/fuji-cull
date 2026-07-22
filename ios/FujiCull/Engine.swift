import Foundation
import Mobile

// Engine wraps the gomobile fuji-cull core: it seeds a fake media corpus,
// boots the engine against the local `dir` backend (no camera / no exec — the
// simulator path), and polls discovery status for the connect screen. The rest
// of the UI talks to the engine over the loopback HTTP API at `baseURL`.
@MainActor
final class Engine: ObservableObject {
    @Published var status: String = "starting…"
    @Published var ready: Bool = false
    @Published var shotCount: Int = 0
    @Published var port: Int = 0
    @Published var log: String = ""
    @Published var startError: String? = nil

    private var engine: MobileEngine?
    private var pollTask: Task<Void, Never>?

    var baseURL: URL? { port > 0 ? URL(string: "http://127.0.0.1:\(port)") : nil }

    // Default import destination: an on-device folder in the app sandbox.
    var defaultImportDest: String {
        FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("imported").path
    }

    func start() {
        guard engine == nil else { return }
        let fm = FileManager.default
        let docs = fm.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let dataDir = docs.appendingPathComponent("engine").path
        let cacheDir = docs.appendingPathComponent("cache").path
        let corpus = docs.appendingPathComponent("fake-corpus").path
        for p in [dataDir, cacheDir, corpus] {
            try? fm.createDirectory(atPath: p, withIntermediateDirectories: true)
        }

        // 6 folders x 40 = 240 synthetic shots; idempotent across launches.
        var seedErr: NSError?
        MobileSeedFakeCorpus(corpus, 6, 40, &seedErr)
        if let seedErr {
            startError = seedErr.localizedDescription
            status = "corpus seed failed"
            return
        }

        var startErr: NSError?
        guard let e = MobileStartLocal(dataDir, cacheDir, corpus, "sim", &startErr) else {
            startError = startErr?.localizedDescription ?? "engine failed to start"
            status = "start failed"
            return
        }
        engine = e
        port = e.port()
        startPolling()
    }

    private func startPolling() {
        pollTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                self?.poll()
                try? await Task.sleep(nanoseconds: 500_000_000)
            }
        }
    }

    private func poll() {
        guard let e = engine else { return }
        ready = e.ready()
        shotCount = e.shotCount()
        status = e.discoveryStatus()
        log = e.fullLog()
    }

    func nudge() { engine?.nudge() }

    deinit { pollTask?.cancel() }
}
