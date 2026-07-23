import Foundation
import AVFoundation
import UIKit

// Video poster frames. iOS has no ffmpeg (no exec), so posters are made
// client-side exactly like the Android build (Posters.kt): download the first
// 8 MB of the clip over /api/videohead — Fuji writes moov plus the opening
// frames at the head, the same slice desktop ffmpeg posters use — patch the
// box the download truncated so a strict container parser accepts the file,
// and extract frame 0 with AVAssetImageGenerator. Videos that fail on good
// data are marked once (.fail3) and never retried; transient fetch failures
// (503 = camera link busy) are retried on later sweep passes.
final class Posters {
    static let shared = Posters()

    /// Posted (on main) with the shot id when a poster lands, so the grid can
    /// reconfigure exactly that cell.
    static let posterReady = Notification.Name("posterReady")

    private let lock = NSLock()
    private var memory: [String: URL] = [:]
    private var failedIDs: Set<String> = []
    private var deferredLogged: Set<String> = []

    private var dir: URL {
        FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("posters")
    }

    private func file(_ shot: Shot) -> URL {
        dir.appendingPathComponent(shot.id.replacingOccurrences(of: "/", with: "_") + ".jpg")
    }

    /// Cheap enough for cell configuration: memory first, one file-exists
    /// check on a miss (memoized either way).
    func cached(_ shot: Shot) -> URL? {
        lock.lock()
        if let f = memory[shot.id] { lock.unlock(); return f }
        if failedIDs.contains(shot.id) { lock.unlock(); return nil }
        lock.unlock()

        let f = file(shot)
        if FileManager.default.fileExists(atPath: f.path) {
            lock.lock(); memory[shot.id] = f; lock.unlock()
            return f
        }
        if FileManager.default.fileExists(atPath: f.path + ".fail3") {
            lock.lock(); failedIDs.insert(shot.id); lock.unlock()
        }
        return nil
    }

    func isFailed(_ shot: Shot) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return failedIDs.contains(shot.id)
    }

    /// Poster cached or permanently undecodable — nothing left to do.
    func resolved(_ shot: Shot) -> Bool { cached(shot) != nil || isFailed(shot) }

    /// Fetch + decode one poster. Serial by design (called from the single
    /// sweep task): one videohead pull on the camera link at a time.
    func load(api: API, shot: Shot) async -> URL? {
        if let f = cached(shot) { return f }
        if isFailed(shot) { return nil }

        let f = file(shot)
        // .MOV extension matters: AVURLAsset infers the container from the
        // path extension and refuses to open unknown ones ("Cannot Open")
        let head = dir.appendingPathComponent(shot.id.replacingOccurrences(of: "/", with: "_") + ".head.MOV")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)

        // fetch the head; failures here are transient (busy link), NOT a
        // verdict on the video — retried on a later sweep pass
        var why = ""
        var fetched = false
        do {
            var req = URLRequest(url: api.videoHeadURL(shot.id))
            req.timeoutInterval = 60
            let (data, resp) = try await URLSession.shared.data(for: req)
            if (resp as? HTTPURLResponse)?.statusCode == 200, !data.isEmpty {
                try data.write(to: head)
                fetched = true
            } else {
                why = "http \((resp as? HTTPURLResponse)?.statusCode ?? -1)"
            }
        } catch {
            why = error.localizedDescription
        }
        if !fetched {
            lock.lock()
            let firstTime = deferredLogged.insert(shot.id).inserted
            lock.unlock()
            if firstTime { await api.logEvent("poster: \(shot.base) deferred (\(why))") }
            return nil
        }

        patchTruncatedBoxes(head)

        var diag = ""
        var poster: URL?
        do {
            let asset = AVURLAsset(url: head)
            let gen = AVAssetImageGenerator(asset: asset)
            gen.appliesPreferredTrackTransform = true
            // frame 0 with a forward tolerance: the nearest sync frame is in
            // the head; seeking anywhere else lands past the 8 MB cliff
            gen.requestedTimeToleranceBefore = .zero
            gen.requestedTimeToleranceAfter = .positiveInfinity
            let (cg, _) = try await gen.image(at: .zero)
            let img = UIImage(cgImage: cg)
            let scaled = img.size.width > 480 ? img.scaled(toWidth: 480) : img
            if let jpeg = scaled.jpegData(compressionQuality: 0.8) {
                let tmp = URL(fileURLWithPath: f.path + ".tmp")
                try jpeg.write(to: tmp)
                _ = try FileManager.default.replaceItemAt(f, withItemAt: tmp)
                poster = f
            } else {
                diag = "jpeg encode failed"
            }
        } catch {
            diag = error.localizedDescription
        }
        try? FileManager.default.removeItem(at: head)

        if let poster {
            await api.logEvent("poster: \(shot.base) ok")
            lock.lock(); memory[shot.id] = poster; lock.unlock()
            await MainActor.run {
                NotificationCenter.default.post(name: Posters.posterReady, object: shot.id)
            }
            return poster
        }
        await api.logEvent("poster: \(shot.base) undecodable (\(diag); marked, no retry)")
        FileManager.default.createFile(atPath: f.path + ".fail3", contents: nil)
        lock.lock(); failedIDs.insert(shot.id); lock.unlock()
        return nil
    }

    /// Rewrites the size of the box the download truncated (mdat, since Fuji
    /// writes moov first) so the container is self-consistent — strict
    /// parsers reject boxes that claim more bytes than the file holds, where
    /// ffmpeg would just read what's there. Box sizes are big-endian.
    private func patchTruncatedBoxes(_ url: URL) {
        guard let fh = try? FileHandle(forUpdating: url) else { return }
        defer { try? fh.close() }
        guard let len = try? fh.seekToEnd() else { return }
        var off: UInt64 = 0
        while off + 8 <= len {
            try? fh.seek(toOffset: off)
            guard let hdr = try? fh.read(upToCount: 16), hdr.count >= 8 else { return }
            var size = UInt64(UInt32(bigEndian: hdr.withUnsafeBytes { $0.load(as: UInt32.self) }))
            var header: UInt64 = 8
            if size == 1, hdr.count >= 16 {
                size = UInt64(bigEndian: hdr.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 8, as: UInt64.self) })
                header = 16
            }
            if size == 0 { size = len - off } // "to end of file"
            if off + size > len {
                let fixed = len - off
                if header == 8 {
                    var be = UInt32(fixed).bigEndian
                    try? fh.seek(toOffset: off)
                    try? fh.write(contentsOf: Data(bytes: &be, count: 4))
                } else {
                    var be = fixed.bigEndian
                    try? fh.seek(toOffset: off + 8)
                    try? fh.write(contentsOf: Data(bytes: &be, count: 8))
                }
                return
            }
            off += size
        }
    }
}

private extension UIImage {
    func scaled(toWidth w: CGFloat) -> UIImage {
        let h = size.height * w / size.width
        return UIGraphicsImageRenderer(size: CGSize(width: w, height: h)).image { _ in
            draw(in: CGRect(x: 0, y: 0, width: w, height: h))
        }
    }
}
