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
        /// open the viewer on the first video once the catalog loads, so
        /// playback can be exercised with no one to tap the screen
        var openVideo: Bool?
        /// walk every video's format description (moov-only, no playback)
        /// and log its HEVC profile — finds 4:2:2 (Rext) clips to play-test
        var codecAudit: Bool?
        /// hold the playback cycle until ICC's crawl finishes — the A/B for
        /// "does a busy camera explain the stalls"
        var waitForCrawl: Bool?
    }

    /// Set when the armed config asks for the video-playback rig.
    private(set) static var openVideoRequested = false
    private(set) static var codecAuditRequested = false
    private(set) static var waitForCrawlRequested = false

    static let autoscrollTick = Notification.Name("debugAutoscrollTick")

    private static var snapIndex = 0
    private static var armed = false
    private static var traceHandle: FileHandle?
    private static let traceLock = NSLock()
    private static var mainMachThread: thread_act_t = 0
    private static var stacksDumped = 0

    /// Appends a breadcrumb to Documents/debug/trace.log, synchronously and
    /// WITHOUT touching the engine — the whole point is to identify a call
    /// that never returns, so the write must precede it and depend on nothing.
    /// The last "→name" with no matching "←name" names the blocker.
    static func trace(_ s: String) {
        guard armed else { return }
        traceLock.lock(); defer { traceLock.unlock() }
        if traceHandle == nil {
            let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
            let url = docs.appendingPathComponent("debug/trace.log")
            FileManager.default.createFile(atPath: url.path, contents: nil)
            traceHandle = try? FileHandle(forWritingTo: url)
        }
        let ts = String(format: "%.3f", Date().timeIntervalSince1970)
        traceHandle?.write("\(ts) \(s)\n".data(using: .utf8)!)
    }

    static func armIfConfigured(log: @escaping (String) -> Void) {
        guard !armed else { return }
        let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let cfgURL = docs.appendingPathComponent("debug-probe.json")
        guard let data = try? Data(contentsOf: cfgURL),
              let cfg = try? JSONDecoder().decode(Config.self, from: data) else { return }
        armed = true
        openVideoRequested = cfg.openVideo ?? false
        codecAuditRequested = cfg.codecAudit ?? false
        waitForCrawlRequested = cfg.waitForCrawl ?? false
        mainMachThread = mach_thread_self() // armIfConfigured runs on main
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
        trace("→snapshot")
        defer { trace("←snapshot") }
        let t0 = Date()
        guard let window = UIApplication.shared.connectedScenes
            .compactMap({ ($0 as? UIWindowScene)?.keyWindow }).first else { return }
        let renderer = UIGraphicsImageRenderer(bounds: window.bounds)
        let img = renderer.image { _ in
            window.drawHierarchy(in: window.bounds, afterScreenUpdates: false)
        }
        let url = dir.appendingPathComponent("snap-\(snapIndex % 12).png")
        snapIndex += 1
        try? img.pngData()?.write(to: url)
        let ms = Int(Date().timeIntervalSince(t0) * 1000)
        if ms > 500 { NSLog("[hang] snapshot took %dms", ms) }
    }

    /// Background thread that measures main-queue responsiveness. On a
    /// 30s-ceiling hang it captures the main thread's stack in-process
    /// (suspend → walk the arm64 fp chain → resume → dladdr), because no
    /// external sampler can reach a device that is debugged wirelessly.
    private static func startWatchdog(log: @escaping (String) -> Void) {
        Thread.detachNewThread {
            Thread.current.name = "debug-probe-watchdog"
            while true {
                let t0 = Date()
                let done = DispatchSemaphore(value: 0)
                DispatchQueue.main.async { done.signal() }
                // wait generously so a long hang reports its true length
                let timedOut = done.wait(timeout: .now() + 30) == .timedOut
                let ms = Int(Date().timeIntervalSince(t0) * 1000)
                if ms > 500 {
                    log(String(format: "debug-probe: MAIN THREAD HANG %dms", ms))
                }
                if timedOut && stacksDumped < 5 {
                    stacksDumped += 1
                    let frames = mainThreadStack()
                    trace("MAIN STACK (dump \(stacksDumped)):")
                    frames.forEach { trace("  " + $0) }
                    log("debug-probe: main stack dumped (\(frames.count) frames) — see debug/trace.log")
                }
                Thread.sleep(forTimeInterval: 0.5)
            }
        }
    }

    /// Suspends the (hung) main thread, walks its frame-pointer chain, and
    /// symbolicates. arm64 only — a frame is [previous fp, lr].
    private static func mainThreadStack() -> [String] {
        let thread = mainMachThread
        guard thread != 0, thread_suspend(thread) == KERN_SUCCESS else { return ["<suspend failed>"] }
        var state = arm_thread_state64_t()
        var count = mach_msg_type_number_t(MemoryLayout<arm_thread_state64_t>.size / MemoryLayout<UInt32>.size)
        let kr = withUnsafeMutablePointer(to: &state) { p in
            p.withMemoryRebound(to: natural_t.self, capacity: Int(count)) {
                thread_get_state(thread, ARM_THREAD_STATE64, $0, &count)
            }
        }
        var pcs: [UInt64] = []
        if kr == KERN_SUCCESS {
            pcs.append(state.__pc)
            if state.__lr != 0 { pcs.append(state.__lr) }
            var fp = state.__fp
            for _ in 0..<48 {
                // stack frames are 16-byte aligned and grow to higher addresses
                guard fp != 0, fp % 16 == 0, fp > 0x1000 else { break }
                let frame = UnsafeRawPointer(bitPattern: UInt(fp))
                guard let frame else { break }
                let prevFP = frame.load(fromByteOffset: 0, as: UInt64.self)
                let lr = frame.load(fromByteOffset: 8, as: UInt64.self)
                if lr == 0 { break }
                pcs.append(lr)
                guard prevFP > fp else { break }
                fp = prevFP
            }
        }
        thread_resume(thread)

        return pcs.map { pc in
            var info = Dl_info()
            // strip pointer authentication bits so dladdr can resolve
            let stripped = UInt(pc & 0x0000_7FFF_FFFF_FFFF)
            guard dladdr(UnsafeRawPointer(bitPattern: stripped), &info) != 0 else {
                return String(format: "0x%llx", pc)
            }
            let mod = info.dli_fname.map { (String(cString: $0) as NSString).lastPathComponent } ?? "?"
            let sym = info.dli_sname.map { String(cString: $0) } ?? "?"
            let off = info.dli_saddr.map { stripped - UInt(bitPattern: $0) } ?? 0
            return "\(mod)  \(sym) + \(off)"
        }
    }
}
