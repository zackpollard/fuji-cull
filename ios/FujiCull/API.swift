import Foundation

// Mirrors the engine's /api/state shot DTO (see internal/cull/server.go).
struct Shot: Identifiable, Decodable, Equatable {
    let id: String
    let folder: String
    let base: String
    let kind: String        // "photo" | "video"
    let date: String?
    let files: [String: String]?  // ext -> filename; drives the RAF chip

    var hasRAF: Bool { files?.keys.contains("RAF") ?? false }
}

struct ImportStatus: Decodable, Equatable {
    let running: Bool
    let phase: String       // idle | copy | hash | upload | validate | done | error
    let done: Int
    let total: Int
    let message: String
    let error: String
    let dest: String
    // Copy and upload run at the same time now, so they are counted apart.
    let uploaded: Int?
}

// What the next import would carry. "keep" behaves as a queue: shots already
// imported drop out, so a finished event is not sent again — and filed into
// the wrong album.
struct PendingImport: Decodable, Equatable {
    let shots: Int
    let imported: Int
}

struct AppState: Decodable {
    let backend: String
    let camera: String?     // "X-H2S 21AQ00123" — the top-bar device chip
    let cursor: Int
    let shots: [Shot]
    let decisions: [String: String]
    let counts: [String: Int]
    let importStatus: ImportStatus?

    enum CodingKeys: String, CodingKey {
        case backend, camera, cursor, shots, decisions, counts
        case importStatus = "import"
    }
}

// /api/status — live link + progress, polled while culling.
struct EngineStatus: Decodable {
    let counts: [String: Int]
    let decisions: [String: String]
    let fetch: [String: String]     // shot id -> fetching | ready | failed
    let bulkSick: Bool
    let partSick: Bool
    let streaming: Bool
    let posters: Bool
    let importStatus: ImportStatus?
    let pending: PendingImport?

    enum CodingKeys: String, CodingKey {
        case counts, decisions, fetch, bulkSick, partSick, streaming, posters, pending
        case importStatus = "import"
    }
}

// /api/thumbs — per-shot thumbnail/orientation/Immich state strings.
struct ThumbInfo: Decodable {
    let states: String      // 0 missing, 1 have, 2 failed, - n/a
    let have: Int
    let orient: String      // EXIF orientation char, 0 unknown
    let immich: String      // '1' = already uploaded
}

// Focus data from the engine's sweep. `scores` grows as it progresses; `best`
// is the sharpest frame of each burst — the only focus comparison that means
// anything, since the raw score tracks how much texture a scene has as much as
// whether it's in focus. Grouping is done engine-side from EXIF capture times.
struct SharpnessInfo: Decodable {
    let scores: [String: Double]
    let best: [String]?
}

// API is a thin client of the engine's loopback HTTP surface — the same
// endpoints the Android app and web UI drive.
final class API {
    let base: URL
    // Engine key for remote (LAN) hosts. Empty for a local loopback engine
    // (no auth). JSON calls send it as the x-api-key header; media URLs carry it
    // as ?key= so mpv / <video> / image loaders authenticate without headers.
    private let key: String
    init(base: URL, key: String = "") { self.base = base; self.key = key }

    /// Appends the engine key to a media URL when talking to a remote host.
    private func keyed(_ url: URL) -> URL {
        guard !key.isEmpty, var c = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        c.queryItems = (c.queryItems ?? []) + [.init(name: "key", value: key)]
        return c.url ?? url
    }

    func fetchState() async throws -> AppState { try await get("api/state") }
    func fetchThumbs() async throws -> ThumbInfo { try await get("api/thumbs") }
    func fetchSharpness() async throws -> SharpnessInfo { try await get("api/sharpness") }
    func fetchStatus() async throws -> EngineStatus { try await get("api/status") }

    func setCursor(_ index: Int) async { await post("api/cursor", ["index": index]) }
    func setThumbHint(_ index: Int) async { await post("api/thumbhint", ["index": index]) }
    func decide(_ id: String, _ decision: String) async {
        await post("api/decision", ["id": id, "decision": decision.isEmpty ? "clear" : decision])
    }
    func startImport(dest: String, album: String, reimport: Bool = false) async {
        await post("api/import", ["dest": dest, "album": album, "reimport": reimport])
    }
    func retryShot(_ id: String) async { await post("api/retry", ["id": id]) }
    func loadVideo(_ id: String) async { await post("api/loadvideo", ["id": id]) }
    func releaseStream() async { await post("api/releasestream", [:]) }
    func rescan() async { await post("api/rescan", [:]) }
    func logEvent(_ msg: String) async { await post("api/log", ["msg": msg]) }

    func thumbURL(_ id: String, orient: Int, tick: Int = 0) -> URL {
        var c = URLComponents(url: base.appendingPathComponent("api/thumb"), resolvingAgainstBaseURL: false)!
        // raw: orientation is applied client-side (free, via
        // UIImage.Orientation), so the URL never varies with EXIF state and
        // URLCache serves repeats without touching the engine
        var q: [URLQueryItem] = [.init(name: "id", value: id), .init(name: "raw", value: "1")]
        if tick > 0 { q.append(.init(name: "rt", value: String(tick))) }
        c.queryItems = q
        return keyed(c.url!)
    }
    func imageURL(_ id: String) -> URL { single("api/image", id) }

    /// A copy of the frame scaled to `max` px on the long edge — an order of
    /// magnitude fewer bytes than the original, which is what lets the viewer
    /// buffer ahead faster than a finger swipes. Zoom still loads imageURL.
    func previewURL(_ id: String, max: Int) -> URL {
        var c = URLComponents(url: base.appendingPathComponent("api/image"), resolvingAgainstBaseURL: false)!
        c.queryItems = [.init(name: "id", value: id), .init(name: "max", value: String(max))]
        return keyed(c.url!)
    }

    func videoURL(_ id: String) -> URL { single("api/video", id) }
    func videoHeadURL(_ id: String) -> URL { single("api/videohead", id) }

    private func single(_ path: String, _ id: String) -> URL {
        var c = URLComponents(url: base.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        c.queryItems = [.init(name: "id", value: id)]
        return keyed(c.url!)
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        var req = URLRequest(url: base.appendingPathComponent(path))
        if !key.isEmpty { req.setValue(key, forHTTPHeaderField: "x-api-key") }
        let (data, _) = try await URLSession.shared.data(for: req)
        return try JSONDecoder().decode(T.self, from: data)
    }
    private func post(_ path: String, _ body: [String: Any]) async {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if !key.isEmpty { req.setValue(key, forHTTPHeaderField: "x-api-key") }
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        _ = try? await URLSession.shared.data(for: req)
    }
}
