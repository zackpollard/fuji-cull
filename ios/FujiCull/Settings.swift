import Foundation

// AppSettings mirrors the Android app's Settings: Immich credentials, session
// name and RAF+JPG stacking, plus iOS-side import destination and a switch to
// force the fake corpus (useful on-device when no camera is attached).
struct AppSettings: Codable, Equatable {
    var immichURL: String = ""
    var immichKey: String = ""
    // sessions are engine-internal, keyed per camera (design decision: no
    // user-named sessions — decisions follow the camera automatically)
    var stack: Bool = false
    var album: String = ""
    var importDest: String = ""
    var forceFake: Bool = false
    // cross-device progress sync (optional): a self-hosted fuji-sync server
    var syncURL: String = ""
    var syncKey: String = ""
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
        if settings.importDest.isEmpty {
            settings.importDest = FileManager.default
                .urls(for: .documentDirectory, in: .userDomainMask)[0]
                .appendingPathComponent("imported").path
        }
    }

    private func save() {
        if let data = try? JSONEncoder().encode(settings) {
            UserDefaults.standard.set(data, forKey: Self.key)
        }
    }
}
