import Foundation
import ImageCaptureCore
import Mobile

// ICCTransport implements the engine's Transport over Apple's ImageCaptureCore —
// the sanctioned PTP/MTP stack on iPadOS, since iOS has neither exec nor usbfs
// for the patched aft-mtp-cli the desktop/Android builds use.
//
// The engine calls these methods synchronously from Go goroutines (never the
// main thread), while ICC is callback-driven; each call bridges with a semaphore
// plus a timeout kept just above the engine's own watchdogs. All camera work is
// serialized through `gate` because the engine — and MTP itself — assume a
// single-threaded link.
final class ICCTransport: NSObject, MobileTransportProtocol {
    static let shared = ICCTransport()

    /// Human-readable link state for the connect screen.
    private(set) var status: String = "looking for a camera…"

    /// Camera-link events, drained into the engine log by Engine.poll() so the
    /// in-app diagnostics screen tells one story — essential when the iPad is
    /// debugged wirelessly (the USB port belongs to the camera).
    private var pending: [String] = []

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

    private let browser = ICDeviceBrowser()
    private let gate = DispatchSemaphore(value: 1)   // one camera op at a time
    private let lock = NSLock()                      // guards the fields below
    private var camera: ICCameraDevice?
    private var catalogComplete = false
    private var filesByID: [String: ICCameraFile] = [:]
    private var folderList: [(dir: String, folder: String)] = []
    private var readyWaiters: [DispatchSemaphore] = []

    /// NNN_FUJI-style folders (the camera's DCIM buckets).
    private static let folderPattern = try! NSRegularExpression(pattern: "^[0-9]{3}[A-Z0-9_]+$")

    // MARK: - lifecycle

    /// Starts device discovery. Safe to call repeatedly.
    func start() {
        browser.delegate = self
        if !browser.isBrowsing { browser.start() }
    }

    // MARK: - MobileTransport

    func connected() -> Bool {
        lock.lock(); defer { lock.unlock() }
        return camera != nil
    }

    func folders() throws -> Data {
        try waitForCatalog()
        lock.lock(); let list = folderList; lock.unlock()
        return try JSONSerialization.data(withJSONObject: list.map { ["dir": $0.dir, "folder": $0.folder] })
    }

    func entries(_ dir: String?) throws -> Data {
        try waitForCatalog()
        guard let dir else { return try JSONSerialization.data(withJSONObject: []) }
        lock.lock()
        let files = filesByID.filter { $0.key.hasPrefix(dir + "/") }
        lock.unlock()

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
    /// readAt in chunks (so demands preempt cleanly); this is the fallback,
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
        file.requestReadData(atOffset: off_t(offset), length: off_t(size)) { data, error in
            out = data
            failure = error
            done.signal()
        }
        // above the engine's own partial-read watchdog so it wins the race
        if done.wait(timeout: .now() + 45) == .timedOut {
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

    private func setStatus(_ s: String) {
        lock.lock(); status = s; lock.unlock()
    }

    /// Blocks until the camera's content catalog is enumerated.
    private func waitForCatalog() throws {
        lock.lock()
        if catalogComplete && camera != nil { lock.unlock(); return }
        guard camera != nil else { lock.unlock(); throw err("no camera attached") }
        let waiter = DispatchSemaphore(value: 0)
        readyWaiters.append(waiter)
        lock.unlock()

        if waiter.wait(timeout: .now() + 180) == .timedOut {
            throw err("timed out enumerating the camera")
        }
        lock.lock(); defer { lock.unlock() }
        guard catalogComplete else { throw err("camera enumeration failed") }
    }

    private func signalReady() {
        lock.lock()
        let waiters = readyWaiters
        readyWaiters.removeAll()
        lock.unlock()
        waiters.forEach { $0.signal() }
    }

    /// Walks the camera's contents, collecting NNN_FUJI folders and their files.
    /// Object IDs are "<dir>/<name>" — opaque to the engine and stable per card.
    private func indexContents() {
        lock.lock(); let cam = camera; lock.unlock()
        guard let cam else { return }

        var files: [String: ICCameraFile] = [:]
        var folders: [(String, String)] = []

        func walk(_ items: [ICCameraItem], path: [String]) {
            for item in items {
                guard let folder = item as? ICCameraFolder else { continue }
                let name = folder.name ?? ""
                let sub = path + [name]
                let range = NSRange(name.startIndex..., in: name)
                if Self.folderPattern.firstMatch(in: name, range: range) != nil {
                    let dir = sub.joined(separator: "/")
                    folders.append((dir, name))
                    for child in folder.contents ?? [] {
                        if let f = child as? ICCameraFile, let fname = f.name {
                            files["\(dir)/\(fname)"] = f
                        }
                    }
                } else {
                    walk(folder.contents ?? [], path: sub)
                }
            }
        }
        walk(cam.contents ?? [], path: [])

        lock.lock()
        filesByID = files
        folderList = folders.map { (dir: $0.0, folder: $0.1) }
        catalogComplete = true
        status = "camera ready — \(folders.count) folders, \(files.count) files"
        lock.unlock()
        note("catalog: \(folders.count) folders, \(files.count) files")
        signalReady()
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
        status = "opening \(cam.name ?? "camera")…"
        lock.unlock()
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
        status = "camera disconnected"
        lock.unlock()
        note("camera detached")
        signalReady() // unblock waiters; they re-check and error out
    }
}

// MARK: - ICCameraDeviceDelegate (inherits ICDeviceDelegate)

extension ICCTransport: ICCameraDeviceDelegate {
    // ICDeviceDelegate
    func didRemove(_ device: ICDevice) {
        lock.lock()
        if device === camera { camera = nil; catalogComplete = false }
        lock.unlock()
        signalReady()
    }

    func device(_ device: ICDevice, didOpenSessionWithError error: Error?) {
        if let error {
            note("open session failed: \(error.localizedDescription)")
            setStatus("camera session failed: \(error.localizedDescription)")
            signalReady()
            return
        }
        note("session open — enumerating the card")
        setStatus("enumerating the card…")
    }

    func device(_ device: ICDevice, didCloseSessionWithError error: Error?) {
        note("session closed")
    }

    // ICCameraDeviceDelegate
    func deviceDidBecomeReady(withCompleteContentCatalog device: ICCameraDevice) {
        note("content catalog complete")
        indexContents()
    }

    func cameraDevice(_ camera: ICCameraDevice, didAdd items: [ICCameraItem]) {}
    func cameraDevice(_ camera: ICCameraDevice, didRemove items: [ICCameraItem]) {}
    func cameraDevice(_ camera: ICCameraDevice, didRenameItems items: [ICCameraItem]) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceiveThumbnail thumbnail: CGImage?,
                      for item: ICCameraItem, error: Error?) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceiveMetadata metadata: [AnyHashable: Any]?,
                      for item: ICCameraItem, error: Error?) {}
    func cameraDeviceDidChangeCapability(_ camera: ICCameraDevice) {}
    func cameraDevice(_ camera: ICCameraDevice, didReceivePTPEvent eventData: Data) {}
    func cameraDeviceDidRemoveAccessRestriction(_ device: ICDevice) {}
    func cameraDeviceDidEnableAccessRestriction(_ device: ICDevice) {}
}
