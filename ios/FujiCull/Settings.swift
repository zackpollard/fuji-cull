import Foundation

// AppSettings mirrors the Android app's Settings: Immich credentials, session
// name and RAF+JPG stacking, plus iOS-side import destination and a switch to
// force the fake corpus (useful on-device when no camera is attached).
struct AppSettings: Codable, Equatable {
    var immichURL: String = ""
    var immichKey: String = ""
    var session: String = ""
    var stack: Bool = false
    var album: String = ""
    var importDest: String = ""
    var forceFake: Bool = false
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
