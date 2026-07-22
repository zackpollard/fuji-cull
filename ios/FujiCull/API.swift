import Foundation

// Mirrors the engine's /api/state shot DTO (see internal/cull/server.go).
struct Shot: Identifiable, Decodable, Equatable {
    let id: String
    let folder: String
    let base: String
    let kind: String        // "photo" | "video"
    let date: String?
}

struct ImportStatus: Decodable, Equatable {
    let running: Bool
    let phase: String       // idle | copy | hash | upload | validate | done | error
    let done: Int
    let total: Int
    let message: String
    let error: String
    let dest: String
}

struct AppState: Decodable {
    let backend: String
    let cursor: Int
    let shots: [Shot]
    let decisions: [String: String]
    let counts: [String: Int]
    let importStatus: ImportStatus?

    enum CodingKeys: String, CodingKey {
        case backend, cursor, shots, decisions, counts
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

    enum CodingKeys: String, CodingKey {
        case counts, decisions, fetch, bulkSick, partSick, streaming, posters
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

// API is a thin client of the engine's loopback HTTP surface — the same
// endpoints the Android app and web UI drive.
final class API {
    let base: URL
    init(base: URL) { self.base = base }

    func fetchState() async throws -> AppState { try await get("api/state") }
    func fetchThumbs() async throws -> ThumbInfo { try await get("api/thumbs") }
    func fetchStatus() async throws -> EngineStatus { try await get("api/status") }

    func setCursor(_ index: Int) async { await post("api/cursor", ["index": index]) }
    func setThumbHint(_ index: Int) async { await post("api/thumbhint", ["index": index]) }
    func decide(_ id: String, _ decision: String) async {
        await post("api/decision", ["id": id, "decision": decision.isEmpty ? "clear" : decision])
    }
    func startImport(dest: String, album: String) async { await post("api/import", ["dest": dest, "album": album]) }
    func retryShot(_ id: String) async { await post("api/retry", ["id": id]) }
    func loadVideo(_ id: String) async { await post("api/loadvideo", ["id": id]) }
    func releaseStream() async { await post("api/releasestream", [:]) }
    func rescan() async { await post("api/rescan", [:]) }
    func logEvent(_ msg: String) async { await post("api/log", ["msg": msg]) }

    func thumbURL(_ id: String, orient: Int, tick: Int = 0) -> URL {
        var c = URLComponents(url: base.appendingPathComponent("api/thumb"), resolvingAgainstBaseURL: false)!
        var q: [URLQueryItem] = [.init(name: "id", value: id)]
        if orient > 1 { q.append(.init(name: "o", value: String(orient))) }
        if tick > 0 { q.append(.init(name: "rt", value: String(tick))) }
        c.queryItems = q
        return c.url!
    }
    func imageURL(_ id: String) -> URL { single("api/image", id) }
    func videoURL(_ id: String) -> URL { single("api/video", id) }
    func videoHeadURL(_ id: String) -> URL { single("api/videohead", id) }

    private func single(_ path: String, _ id: String) -> URL {
        var c = URLComponents(url: base.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        c.queryItems = [.init(name: "id", value: id)]
        return c.url!
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        let (data, _) = try await URLSession.shared.data(from: base.appendingPathComponent(path))
        return try JSONDecoder().decode(T.self, from: data)
    }
    private func post(_ path: String, _ body: [String: Any]) async {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        _ = try? await URLSession.shared.data(for: req)
    }
}
