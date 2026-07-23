import SwiftUI

@main
struct FujiCullApp: App {
    @StateObject private var settings = SettingsStore()
    @StateObject private var engine = Engine()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(engine)
                .environmentObject(settings)
                .onAppear {
                    ThumbCache.bumpURLCache()
                    engine.start(settings.settings)
                    DebugProbe.armIfConfigured { [weak engine] msg in
                        NSLog("%@", msg)
                        engine?.logEvent(msg)
                    }
                }
                .preferredColorScheme(.dark)
        }
    }
}

// RootView shows the connect screen until discovery completes, then the culling
// grid. Keyed on the engine epoch so a settings save (which restarts the engine
// on a new port) rebuilds the whole tree — same approach as the Android app.
struct RootView: View {
    @EnvironmentObject var engine: Engine

    var body: some View {
        Group {
            if engine.ready {
                GridView().id(engine.epoch)
            } else {
                ConnectView()
            }
        }
    }
}
