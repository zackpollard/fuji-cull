import Foundation

// AppSettings mirrors the Android app's Settings: Immich credentials, session
// name and RAF+JPG stacking, plus a switch to force the fake corpus (useful
// on-device when no camera is attached).
//
// The import destination is deliberately NOT here: on iOS it can only ever be
// a folder inside our own container, and a stored absolute path breaks on the
// next app update (the container UUID changes). Engine.importDest resolves it.
struct AppSettings: Codable, Equatable {
    var immichURL: String = ""
    var immichKey: String = ""
    // sessions are engine-internal, keyed per camera (design decision: no
    // user-named sessions — decisions follow the camera automatically)
    var stack: Bool = false
    var album: String = ""
    var forceFake: Bool = false
    // cross-device progress sync (optional): a self-hosted fuji-sync server
    var syncURL: String = ""
    var syncKey: String = ""
    // Remote camera host: when set, the app is a thin client of an engine
    // running elsewhere (the machine the camera is plugged into) instead of
    // starting its own. Empty = use this device's camera (the default).
    var remoteURL: String = ""
    var remoteKey: String = ""

    init() {}

    // Tolerant decoder: Swift's synthesized Decodable throws on any missing key,
    // so simply adding a field would make an OLDER saved blob fail to decode and
    // silently reset every setting. decodeIfPresent keeps old data intact and
    // fills new fields with their defaults.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        immichURL = try c.decodeIfPresent(String.self, forKey: .immichURL) ?? ""
        immichKey = try c.decodeIfPresent(String.self, forKey: .immichKey) ?? ""
        stack = try c.decodeIfPresent(Bool.self, forKey: .stack) ?? false
        album = try c.decodeIfPresent(String.self, forKey: .album) ?? ""
        forceFake = try c.decodeIfPresent(Bool.self, forKey: .forceFake) ?? false
        syncURL = try c.decodeIfPresent(String.self, forKey: .syncURL) ?? ""
        syncKey = try c.decodeIfPresent(String.self, forKey: .syncKey) ?? ""
        remoteURL = try c.decodeIfPresent(String.self, forKey: .remoteURL) ?? ""
        remoteKey = try c.decodeIfPresent(String.self, forKey: .remoteKey) ?? ""
    }
}

@MainActor
final class SettingsStore: ObservableObject {
    @Published var settings: AppSettings {
        didSet { save() }
    }

    private static let key = "fujicull.settings"

    init() {
        if let data = UserDefaults.standard.data(forKey: Self.key),
           let s = try? JSONDecoder().decode(AppSettings.self, from: data) {
            settings = s
        } else {
            settings = AppSettings()
        }
    }

    private func save() {
        if let data = try? JSONEncoder().encode(settings) {
            UserDefaults.standard.set(data, forKey: Self.key)
        }
    }
}
