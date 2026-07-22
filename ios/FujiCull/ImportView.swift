import SwiftUI

// ImportView ports the Android/web import dialog: choose a destination, kick off
// the engine's import pipeline over the keepers, and watch phase/progress. On
// iOS the destination is a folder in the app sandbox (Immich upload rides the
// same pipeline once configured in Settings).
struct ImportView: View {
    @ObservedObject var model: GridModel
    let defaultDest: String
    @Environment(\.dismiss) private var dismiss
    @State private var dest: String = ""

    private var keepers: Int { model.counts["keep"] ?? 0 }
    private var status: ImportStatus? { model.importStatus }
    private var running: Bool { status?.running ?? false }

    var body: some View {
        NavigationStack {
            Form {
                Section("Destination") {
                    TextField("path", text: $dest)
                        .font(.system(.footnote, design: .monospaced))
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                }

                Section {
                    Button {
                        model.startImport(dest: dest, album: "")
                    } label: {
                        Label("Import \(keepers) keeper\(keepers == 1 ? "" : "s")",
                              systemImage: "square.and.arrow.down")
                    }
                    .disabled(keepers == 0 || running)
                } footer: {
                    Text("Copies kept shots to the destination. Rejects and undecided are left on the card.")
                }

                if let s = status, s.running || s.phase == "done" || s.phase == "error" {
                    Section("Progress") {
                        LabeledContent("Phase", value: s.phase)
                        if s.total > 0 {
                            ProgressView(value: Double(s.done), total: Double(s.total))
                            Text("\(s.done)/\(s.total)").font(.system(.caption, design: .monospaced))
                        }
                        if !s.message.isEmpty {
                            Text(s.message).font(.caption).foregroundStyle(.secondary)
                        }
                        if !s.error.isEmpty {
                            Text(s.error).font(.caption).foregroundStyle(.red)
                        }
                    }
                }
            }
            .navigationTitle("Import")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) { Button("Done") { dismiss() } }
            }
            .onAppear { if dest.isEmpty { dest = defaultDest } }
        }
    }
}
