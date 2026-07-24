import SwiftUI

// SettingsView ports the Android settings screen: Immich credentials, session
// name, RAF+JPG stacking, plus the iOS import destination and a fake-corpus
// override. Saving restarts the engine (the camera link is re-established).
struct SettingsView: View {
    @EnvironmentObject var engine: Engine
    @EnvironmentObject var store: SettingsStore
    @Environment(\.dismiss) private var dismiss

    @State private var draft = AppSettings()
    @State private var loaded = false
    @State private var showLog = false
    @State private var confirmRescan = false
    // UI-only preference: applies immediately, no engine restart (unlike the
    // draft fields, which are saved and restart the camera link).
    @AppStorage("viewerAnimations") private var viewerAnimations = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("https://immich.example.com", text: $draft.immichURL)
                        .textContentType(.URL)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                    SecureField("immich api key", text: $draft.immichKey)
                    Toggle("Stack RAF+JPG pairs after upload", isOn: $draft.stack)
                } header: {
                    Text("Immich")
                } footer: {
                    Text("Leave the URL or key empty to import without uploading.")
                }

                Section {
                    TextField("http://192.168.1.50:8787", text: $draft.remoteURL)
                        .textContentType(.URL)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                        .keyboardType(.URL)
                    SecureField("engine key", text: $draft.remoteKey)
                } header: {
                    Text("Camera source")
                } footer: {
                    Text("Leave empty to use this device's camera. Set a URL to browse a camera plugged into another machine (that engine must be started with --engine-key). Saving reconnects.")
                }

                Section("Import destination") {
                    TextField("path", text: $draft.importDest)
                        .font(.system(.footnote, design: .monospaced))
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                }

                Section {
                    Toggle("Animate photo transitions", isOn: $viewerAnimations)
                } header: {
                    Text("Viewer")
                } footer: {
                    Text("Off cuts instantly between photos so you can compare frames in a burst. On slides each page in. Applies immediately.")
                }

                Section {
                    LabeledContent("Link", value: {
                        switch engine.mode {
                        case .camera: return "camera (ImageCaptureCore)"
                        case .remote: return "remote host"
                        case .fake: return "fake corpus"
                        case .none: return "—"
                        }
                    }())
                    LabeledContent("Loopback", value: ":\(engine.port)")
                    LabeledContent("Shots", value: "\(engine.shotCount)")
                    Toggle(isOn: $draft.forceFake) {
                        Label {
                            Text("Force fake corpus").foregroundStyle(DS.reject)
                        } icon: {
                            Image(systemName: "exclamationmark.triangle.fill")
                                .foregroundStyle(DS.reject)
                        }
                    }
                    .tint(DS.reject)
                } header: {
                    Text("Developer")
                } footer: {
                    // design: quarantined — it hides the real camera and fills
                    // the grid with synthetic shots
                    Text("Hides the real camera and fills the grid with synthetic shots. Turn OFF before a real shoot.")
                        .foregroundStyle(DS.reject.opacity(0.85))
                }

                Section {
                    Button { showLog = true } label: { Label("View log", systemImage: "terminal") }
                    Button(role: .destructive) { confirmRescan = true } label: {
                        Label("Full rescan", systemImage: "arrow.clockwise")
                    }
                } footer: {
                    Text("Full rescan re-reads the whole camera index — use after deleting in-camera or swapping cards.")
                }
            }
            .navigationTitle("Settings")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Save") {
                        store.settings = draft
                        engine.restart(draft)   // saving restarts the engine
                        dismiss()
                    }.bold()
                }
            }
            .sheet(isPresented: $showLog) { LogSheet(engine: engine) }
            .alert("Full rescan?", isPresented: $confirmRescan) {
                Button("Rescan", role: .destructive) {
                    Task {
                        if let base = engine.baseURL { await API(base: base, key: engine.apiKey).rescan() }
                        store.settings = draft
                        engine.restart(draft)
                        dismiss()
                    }
                }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("Drops the catalog cache and re-reads the camera index.")
            }
            .onAppear {
                if !loaded { draft = store.settings; loaded = true }
            }
        }
    }
}
