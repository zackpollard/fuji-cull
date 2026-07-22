import SwiftUI
import UIKit

// DebugProbe is a self-serve diagnostics rig for debugging the app on a
// device that can't be observed directly (wireless debugging, USB port
// occupied by the camera). It is inert unless Documents/debug-probe.json
// exists — push one with devicectl to arm it, delete it to disarm:
//
//   {"snapshotEvery": 5, "autoscroll": true}
//
// Armed, it:
//  - saves a PNG of the key window to Documents/debug/snap-N.png on a timer
//    (ring of 12) — pullable over devicectl for remote eyes on the UI
//  - posts .debugAutoscroll ticks that GridView turns into programmatic
//    scrolling, so "scroll into an uncached region" reproduces hands-free
//  - runs a main-thread watchdog: a background thread pings the main queue
//    twice a second and logs any response gap over 500ms to the engine log,
//    turning "the app froze" into a measured, timestamped fact
enum DebugProbe {
    struct Config: Decodable {
        var snapshotEvery: Double?
        var autoscroll: Bool?
    }

    static let autoscrollTick = Notification.Name("debugAutoscrollTick")

    private static var snapIndex = 0
    private static var armed = false

    static func armIfConfigured(log: @escaping (String) -> Void) {
        guard !armed else { return }
        let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let cfgURL = docs.appendingPathComponent("debug-probe.json")
        guard let data = try? Data(contentsOf: cfgURL),
              let cfg = try? JSONDecoder().decode(Config.self, from: data) else { return }
        armed = true
        log("debug-probe: armed (snapshotEvery=\(cfg.snapshotEvery ?? 0) autoscroll=\(cfg.autoscroll ?? false))")

        let debugDir = docs.appendingPathComponent("debug")
        try? FileManager.default.createDirectory(at: debugDir, withIntermediateDirectories: true)

        if let every = cfg.snapshotEvery, every > 0 {
            Timer.scheduledTimer(withTimeInterval: every, repeats: true) { _ in
                snapshot(into: debugDir)
            }
        }
        if cfg.autoscroll == true {
            Timer.scheduledTimer(withTimeInterval: 2.5, repeats: true) { _ in
                NotificationCenter.default.post(name: autoscrollTick, object: nil)
            }
        }
        startWatchdog(log: log)
    }

    /// Renders the key window into Documents/debug/snap-N.png (ring of 12).
    private static func snapshot(into dir: URL) {
        guard let window = UIApplication.shared.connectedScenes
            .compactMap({ ($0 as? UIWindowScene)?.keyWindow }).first else { return }
        let renderer = UIGraphicsImageRenderer(bounds: window.bounds)
        let img = renderer.image { _ in
            window.drawHierarchy(in: window.bounds, afterScreenUpdates: false)
        }
        let url = dir.appendingPathComponent("snap-\(snapIndex % 12).png")
        snapIndex += 1
        try? img.pngData()?.write(to: url)
    }

    /// Background thread that measures main-queue responsiveness.
    private static func startWatchdog(log: @escaping (String) -> Void) {
        Thread.detachNewThread {
            Thread.current.name = "debug-probe-watchdog"
            while true {
                let t0 = Date()
                let done = DispatchSemaphore(value: 0)
                DispatchQueue.main.async { done.signal() }
                // wait generously so a long hang reports its true length
                _ = done.wait(timeout: .now() + 30)
                let ms = Int(Date().timeIntervalSince(t0) * 1000)
                if ms > 500 {
                    log(String(format: "debug-probe: MAIN THREAD HANG %dms", ms))
                }
                Thread.sleep(forTimeInterval: 0.5)
            }
        }
    }
}
