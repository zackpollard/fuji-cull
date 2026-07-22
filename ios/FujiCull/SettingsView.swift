import SwiftUI

// SettingsView: engine/session info and culling stats. Camera (ICCTransport)
// and Immich configuration land here in the real-device build; in the simulator
// the engine runs against the bundled fake corpus.
struct SettingsView: View {
    @ObservedObject var model: GridModel
    @ObservedObject var engine: Engine
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            Form {
                Section("Engine") {
                    LabeledContent("Backend", value: "fake (dir)")
                    LabeledContent("Loopback", value: ":\(engine.port)")
                    LabeledContent("Shots", value: "\(model.shots.count)")
                    LabeledContent("Thumbnails", value: "\(model.haveThumbs)/\(model.shots.count)")
                }
                Section("Culling") {
                    LabeledContent("Keep", value: "\(model.counts["keep"] ?? 0)")
                    LabeledContent("Reject", value: "\(model.counts["reject"] ?? 0)")
                    LabeledContent("Undecided", value: "\(model.counts["undecided"] ?? 0)")
                }
                Section("Import") {
                    Text(engine.defaultImportDest)
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(.secondary)
                }
                Section {
                    LabeledContent("Camera", value: "real-device build")
                    LabeledContent("Immich", value: "not configured")
                } header: {
                    Text("Not in the simulator")
                } footer: {
                    Text("ImageCaptureCore camera transport and Immich upload are wired in the on-device build.")
                }
            }
            .navigationTitle("Settings")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) { Button("Done") { dismiss() } }
            }
        }
    }
}
