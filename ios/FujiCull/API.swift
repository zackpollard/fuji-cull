import Foundation

// Mirrors the engine's /api/state shot DTO (see internal/cull/server.go).
struct Shot: Identifiable, Decodable, Equatable {
    let id: String
    let folder: String
    let base: String
    let kind: String        // "photo" | "video"
    let date: String?
}

struct ImportStatus: Decodable {
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

struct ThumbInfo: Decodable {
    let states: String      // one char per shot: 0 missing, 1 have, 2 failed, - n/a
    let have: Int
    let orient: String      // one char per shot: EXIF orientation, 0 unknown
}

// API is a thin client of the engine's loopback HTTP surface.
final class API {
    let base: URL
    init(base: URL) { self.base = base }

    func fetchState() async throws -> AppState { try await get("api/state") }
    func fetchThumbs() async throws -> ThumbInfo { try await get("api/thumbs") }

    func setCursor(_ index: Int) async { await post("api/cursor", ["index": index]) }
    func setThumbHint(_ index: Int) async { await post("api/thumbhint", ["index": index]) }
    func decide(_ id: String, _ decision: String) async { await post("api/decision", ["id": id, "decision": decision]) }
    func startImport(dest: String, album: String) async { await post("api/import", ["dest": dest, "album": album]) }

    func thumbURL(_ id: String, orient: Int) -> URL {
        var c = URLComponents(url: base.appendingPathComponent("api/thumb"), resolvingAgainstBaseURL: false)!
        c.queryItems = [.init(name: "id", value: id), .init(name: "o", value: String(orient))]
        return c.url!
    }
    func imageURL(_ id: String) -> URL {
        var c = URLComponents(url: base.appendingPathComponent("api/image"), resolvingAgainstBaseURL: false)!
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
