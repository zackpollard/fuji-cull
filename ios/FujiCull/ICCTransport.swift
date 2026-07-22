import Foundation
import ImageCaptureCore
import Mobile

// ICCTransport implements the engine's Transport over Apple's ImageCaptureCore —
// the sanctioned PTP/MTP stack on iPadOS, since iOS has neither exec nor usbfs
// for the patched aft-mtp-cli the desktop/Android builds use.
//
// The link is object-level, not raw PTP, and not by choice: ICC's
// requestSendPTPCommand never delivers a callback on iPadOS (verified on 26.5
// against the X-H2S — every container encoding, both API variants, both
// threads, before and after the catalog; requests vanish without even an
// error). What does work is ICC's own machinery: it enumerates the card as
// soon as a session opens (~4 min for a 19k-file card, progress reported
// here), and ICCameraFile.requestReadData serves partial reads — so the
// engine's chunked pull/preempt model survives unchanged.
//
// `connected()` stays false until the catalog is enumerated; the engine's
// discover loop just retries, so nothing here ever blocks waiting for the
// catalog. The engine calls these methods from Go goroutines (never the main
// thread); reads bridge to ICC's callbacks with a semaphore plus a timeout
// above the engine's own watchdogs, serialized because MTP is a
// single-threaded link.
final class ICCTransport: NSObject, MobileTransportProtocol {
    static let shared = ICCTransport()

    /// Human-readable link state for the connect screen.
    private(set) var status: String = "looking for a camera…"

    /// Camera-link events, drained into the engine log by Engine.poll() so the
    /// in-app diagnostics screen tells one story — essential when the iPad is
    /// debugged wirelessly (the USB port belongs to the camera).
    private var pending: [String] = []

    private let browser = ICDeviceBrowser()
    private let gate = DispatchSemaphore(value: 1)   // one camera op at a time
    private let lock = NSLock()
    private var camera: ICCameraDevice?
    private var catalogComplete = false
    private var filesByID: [String: ICCameraFile] = [:]
    private var folderList: [(dir: String, folder: String)] = []
    private var progressTimer: DispatchSourceTimer?
    private var lastCatalogPct: Int = -1
    private var addedCount = 0

    /// NNN_FUJI-style folders (the camera's DCIM buckets).
    private static let folderPattern = try! NSRegularExpression(pattern: "^[0-9]{3}[A-Z0-9_]+$")

    // MARK: - lifecycle

    /// Starts device discovery. Safe to call repeatedly.
    func start() {
        browser.delegate = self
        if !browser.isBrowsing { browser.start() }
    }

    func drainLog() -> [String] {
        lock.lock(); defer { lock.unlock() }
        let out = pending
        pending.removeAll()
        return out
    }

    private func note(_ msg: String) {
        NSLog("[icc] %@", msg)
        lock.lock()
        pending.append(msg)
        if pending.count > 300 { pending.removeFirst(pending.count - 300) }
        lock.unlock()
    }

    private func setStatus(_ s: String) { lock.lock(); status = s; lock.unlock() }

    // MARK: - MobileTransport

    /// True only once the card is enumerated — the engine's discover loop
    /// polls this, so folders()/entries() are never called before the index
    /// exists and nothing has to block.
    func connected() -> Bool {
        lock.lock(); defer { lock.unlock() }
        return camera != nil && catalogComplete
    }

    func folders() throws -> Data {
        lock.lock(); let list = folderList; let ready = catalogComplete; lock.unlock()
        guard ready else { throw err("camera index not ready") }
        return try JSONSerialization.data(withJSONObject: list.map { ["dir": $0.dir, "folder": $0.folder] })
    }

    func entries(_ dir: String?) throws -> Data {
        guard let dir else { return try JSONSerialization.data(withJSONObject: []) }
        lock.lock()
        let ready = catalogComplete
        let files = filesByID.filter { $0.key.hasPrefix(dir + "/") }
        lock.unlock()
        guard ready else { throw err("camera index not ready") }

        let iso = DateFormatter()
        iso.dateFormat = "yyyy-MM-dd"
        let payload: [[String: Any]] = files.map { id, f in
            [
                "objectID": id,
                "name": f.name ?? "",
                "size": Int64(f.fileSize),
                "date": f.creationDate.map { iso.string(from: $0) } ?? "",
            ]
        }
        return try JSONSerialization.data(withJSONObject: payload)
    }

    func read(at objectID: String?, offset: Int64, size: Int64) throws -> Data {
        guard let objectID, let file = self.file(for: objectID) else {
            throw err("unknown object \(objectID ?? "")")
        }
        gate.wait(); defer { gate.signal() }
        return try readChunk(file, offset: offset, size: size)
    }

    /// Whole-object pull. The prefetcher normally streams images through
    /// read(at:) in chunks (so demands preempt cleanly); this is the fallback,
    /// implemented on the same partial-read primitive.
    func download(_ objectID: String?, destPath: String?) throws {
        guard let objectID, let destPath, let file = self.file(for: objectID) else {
            throw err("unknown object \(objectID ?? "")")
        }
        gate.wait(); defer { gate.signal() }

        let url = URL(fileURLWithPath: destPath)
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(),
                                                 withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: destPath, contents: nil)
        guard let handle = FileHandle(forWritingAtPath: destPath) else {
            throw err("cannot write \(destPath)")
        }
        defer { try? handle.close() }

        let total = Int64(file.fileSize)
        let chunk: Int64 = 8 << 20
        var off: Int64 = 0
        while off < total {
            let want = min(chunk, total - off)
            let data = try readChunk(file, offset: off, size: want)
            if data.isEmpty { break }
            try handle.write(contentsOf: data)
            off += Int64(data.count)
            if Int64(data.count) < want { break } // short read: end of object
        }
    }

    // MARK: - internals

    private func readChunk(_ file: ICCameraFile, offset: Int64, size: Int64) throws -> Data {
        var out: Data?
        var failure: Error?
        let done = DispatchSemaphore(value: 0)
        // ImageCaptureCore is a run-loop framework: issue from the main thread
        // (a lesson from the passthrough debugging — background-queue requests
        // can vanish without a callback).
        DispatchQueue.main.async {
            file.requestReadData(atOffset: off_t(offset), length: off_t(size)) { data, error in
                out = data
                failure = error
                done.signal()
            }
        }
        // above the engine's own partial-read watchdog so it wins the race
        if done.wait(timeout: .now() + 45) == .timedOut {
            note("read \(file.name ?? "?") @\(offset)+\(size): no callback after 45s")
            throw err("partial read timed out (offset \(offset), \(size) bytes)")
        }
        if let failure { throw failure }
        guard let out else { throw err("partial read returned no data") }
        return out
    }

    private func file(for id: String) -> ICCameraFile? {
        lock.lock(); defer { lock.unlock() }
        return filesByID[id]
    }

    private func err(_ msg: String) -> NSError {
        NSError(domain: "ICCTransport", code: 1, userInfo: [NSLocalizedDescriptionKey: msg])
    }

    /// Reports ICC's catalog progress while it enumerates, so the connect
    /// screen and the engine log show movement instead of an opaque stall.
    private func startCatalogProgress() {
        progressTimer?.cancel()
        let t = DispatchSource.makeTimerSource(queue: .global())
        t.schedule(deadline: .now() + 2, repeating: 3)
        t.setEventHandler { [weak self] in
            guard let self else { return }
            self.lock.lock()
            let cam = self.camera
            let done = self.catalogComplete
            self.lock.unlock()
            guard let cam, !done else { self.progressTimer?.cancel(); return }
            let pct = Int(cam.contentCatalogPercentCompleted)
            self.setStatus("camera indexing the card — \(pct)%")
            // only on change: a stuck catalog otherwise floods the log
            self.lock.lock()
            let changed = pct != self.lastCatalogPct
            self.lastCatalogPct = pct
            self.lock.unlock()
            if changed { self.note("ICC catalog \(pct)%") }
        }
        t.resume()
        progressTimer = t
    }

    private func matchesFolderPattern(_ name: String) -> Bool {
        let range = NSRange(name.startIndex..., in: name)
        return Self.folderPattern.firstMatch(in: name, range: range) != nil
    }

    /// Ancestry of a file as path components, oldest first ("SLOT 1/DCIM/151_FUJI").
    private func folderPath(of item: ICCameraItem) -> [String] {
        var parts: [String] = []
        var cur = item.parentFolder
        var hops = 0
        while let f = cur, hops < 16 {
            parts.insert(f.name ?? "?", at: 0)
            cur = f.parentFolder
            hops += 1
        }
        return parts
    }

    /// Indexes the enumerated card, collecting NNN_FUJI folders and their files.
    /// Object IDs are "<dir>/<name>" — opaque to the engine and stable per card.
    ///
    /// Files are grouped by climbing each one's parentFolder chain rather than
    /// walking cam.contents: iPadOS delivered an empty contents tree for a card
    /// it had just enumerated 19k files from, while mediaFiles held them all.
    private func indexContents() {
        lock.lock(); let cam = camera; lock.unlock()
        guard let cam else { return }

        // Flat view first; fall back to flattening the contents tree.
        var all: [ICCameraFile] = (cam.mediaFiles ?? []).compactMap { $0 as? ICCameraFile }
        if all.isEmpty {
            func flatten(_ items: [ICCameraItem]) {
                for item in items {
                    if let f = item as? ICCameraFile { all.append(f) }
                    if let d = item as? ICCameraFolder { flatten(d.contents ?? []) }
                }
            }
            flatten(cam.contents ?? [])
        }
        note("index: contents=\(cam.contents?.count ?? -1) mediaFiles=\(cam.mediaFiles?.count ?? -1) flat=\(all.count)")
        // shape sample, for when the layout surprises us again
        for item in all.prefix(3) {
            note("index sample: \(type(of: item)) name=\(item.name ?? "nil") parents=\(folderPath(of: item).joined(separator: "/"))")
        }

        var files: [String: ICCameraFile] = [:]
        var buckets: [String: String] = [:]   // dir -> folder display name
        var unmatched: [String: Int] = [:]    // non-NNN_FUJI parents, for the log
        for f in all {
            guard let fname = f.name else { continue }
            let parents = folderPath(of: f)
            guard let folder = parents.last, matchesFolderPattern(folder) else {
                unmatched[parents.last ?? "(no parent)", default: 0] += 1
                continue
            }
            let dir = parents.joined(separator: "/")
            buckets[dir] = folder
            files["\(dir)/\(fname)"] = f
        }
        if !unmatched.isEmpty {
            note("index: skipped non-FUJI parents \(unmatched)")
        }
        let folders = buckets.sorted { $0.key < $1.key }.map { (dir: $0.key, folder: $0.value) }

        lock.lock()
        filesByID = files
        folderList = folders
        catalogComplete = true
        status = "camera ready — \(folders.count) folders, \(files.count) files"
        lock.unlock()
        note("catalog indexed — \(folders.count) folders, \(files.count) files")
    }
}

// MARK: - ICDeviceBrowserDelegate

extension ICCTransport: ICDeviceBrowserDelegate {
    func deviceBrowser(_ browser: ICDeviceBrowser, didAdd device: ICDevice, moreComing: Bool) {
        guard let cam = device as? ICCameraDevice else { return }
        note("camera attached: \(cam.name ?? "?")")
        lock.lock()
        camera = cam
        catalogComplete = false
        addedCount = 0
        lock.unlock()
        setStatus("opening \(cam.name ?? "camera")…")
        cam.delegate = self
        cam.requestOpenSession()
    }

    func deviceBrowser(_ browser: ICDeviceBrowser, didRemove device: ICDevice, moreGoing: Bool) {
        lock.lock()
        guard device === camera else { lock.unlock(); return }
        camera = nil
        catalogComplete = false
        filesByID.removeAll()
        folderList.removeAll()
        lock.unlock()
        note("camera detached")
        setStatus("camera disconnected")
    }
}

// MARK: - ICCameraDeviceDelegate (inherits ICDeviceDelegate)

extension ICCTransport: ICCameraDeviceDelegate {
    func didRemove(_ device: ICDevice) {
        lock.lock()
        if device === camera {
            camera = nil
            catalogComplete = false
            filesByID.removeAll()
            folderList.removeAll()
        }
        lock.unlock()
    }

    func device(_ device: ICDevice, didOpenSessionWithError error: Error?) {
        if let error {
            note("open session failed: \(error.localizedDescription)")
            setStatus("camera session failed: \(error.localizedDescription)")
            return
        }
        note("session open — waiting for the card catalog")
        setStatus("camera indexing the card…")
        startCatalogProgress()
    }

    func device(_ device: ICDevice, didEncounterError error: Error?) {
        note("device error: \(error?.localizedDescription ?? "?")")
    }

    func device(_ device: ICDevice, didCloseSessionWithError error: Error?) {
        lock.lock(); catalogComplete = false; lock.unlock()
        note("session closed")
    }

    func deviceDidBecomeReady(withCompleteContentCatalog device: ICCameraDevice) {
        progressTimer?.cancel()
        note("ICC catalog complete")
        indexContents()
    }

    func cameraDevice(_ camera: ICCameraDevice, didAdd items: [ICCameraItem]) {
        // one line per k items, not 19k lines per card
        lock.lock()
        addedCount += items.count
        let n = addedCount
        lock.unlock()
        if n % 2000 < items.count { note("ICC enumerated \(n) items…") }
    }
    func cameraDevice(_ camera: ICCameraDevice, didRemove items: [ICCameraItem]) {}
    func cameraDevice(_ camera: ICCameraDevice, didRenameItems items: [ICCameraItem]) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceiveThumbnail thumbnail: CGImage?,
                      for item: ICCameraItem, error: Error?) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceiveMetadata metadata: [AnyHashable: Any]?,
                      for item: ICCameraItem, error: Error?) {}
    func cameraDeviceDidChangeCapability(_ camera: ICCameraDevice) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceivePTPEvent eventData: Data) {}
    func cameraDeviceDidRemoveAccessRestriction(_ device: ICDevice) { note("access restriction removed") }
    func cameraDeviceDidEnableAccessRestriction(_ device: ICDevice) { note("access restriction enabled") }
}
