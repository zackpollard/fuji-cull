import SwiftUI

@main
struct FujiCullApp: App {
    @StateObject private var engine = Engine()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(engine)
                .onAppear { engine.start() }
                .preferredColorScheme(.dark)
        }
    }
}

// RootView shows the connect screen until discovery completes, then the app
// proper (grid — Phase 2). For now readiness reveals a placeholder home.
struct RootView: View {
    @EnvironmentObject var engine: Engine

    var body: some View {
        if engine.ready {
            GridView()
        } else {
            ConnectView()
        }
    }
}
