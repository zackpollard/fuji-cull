import SwiftUI

// ImportView ports the Android/web import dialog: choose a destination, kick off
// the engine's import pipeline over the keepers, and watch phase/progress. On
// iOS the destination is a folder in the app sandbox (Immich upload rides the
// same pipeline once configured in Settings).
struct ImportView: View {
    @ObservedObject var model: GridModel
    let defaultDest: String
    var album: String = ""
    @Environment(\.dismiss) private var dismiss
    @State private var dest: String = ""
    @State private var albumName: String = ""

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

                Section("Immich album (optional)") {
                    TextField("album name", text: $albumName)
                        .autocorrectionDisabled()
                }

                Section {
                    Button {
                        model.startImport(dest: dest, album: albumName)
                    } label: {
                        Label("Import \(keepers) keeper\(keepers == 1 ? "" : "s")",
                              systemImage: "square.and.arrow.down")
                    }
                    .disabled(keepers == 0 || running)
                } footer: {
                    Text("Copies kept shots to the destination. Rejects and undecided are left on the card.")
                }

                if let s = status, s.running || s.phase == "done" || s.phase == "error" {
                    // design: the four phases as explicit rows — done ✓ green,
                    // active amber with progress, todo muted
                    Section("Progress") {
                        let phases = ["copy", "hash", "upload", "validate"]
                        let activeIdx = phases.firstIndex(of: s.phase) ?? (s.phase == "done" ? phases.count : 0)
                        ForEach(Array(phases.enumerated()), id: \.offset) { i, p in
                            HStack {
                                if i < activeIdx || s.phase == "done" {
                                    Image(systemName: "checkmark").foregroundStyle(DS.keep)
                                } else if i == activeIdx && s.running {
                                    ProgressView().controlSize(.small).tint(DS.amber)
                                } else {
                                    Image(systemName: "circle").foregroundStyle(DS.text3)
                                }
                                Text(p.uppercased()).font(DS.label(13))
                                    .foregroundStyle(i <= activeIdx ? DS.text : DS.text3)
                                Spacer()
                                if i == activeIdx && s.running && s.total > 0 {
                                    Text("\(s.done)/\(s.total)").font(DS.micro())
                                        .foregroundStyle(DS.text2)
                                }
                            }
                        }
                        if s.total > 0 && s.running {
                            ProgressView(value: Double(s.done), total: Double(s.total)).tint(DS.amber)
                        }
                        if !s.message.isEmpty {
                            Text(s.message).font(DS.body(13)).foregroundStyle(DS.text2)
                        }
                        if !s.error.isEmpty {
                            Text(s.error).font(DS.body(13)).foregroundStyle(DS.reject)
                        }
                        if s.phase == "done" {
                            Label("import complete", systemImage: "checkmark.circle.fill")
                                .font(DS.emphasis(14)).foregroundStyle(DS.keep)
                        }
                    }
                }
            }
            .navigationTitle("Import")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) { Button("Done") { dismiss() } }
            }
            .onAppear {
                if dest.isEmpty { dest = defaultDest }
                if albumName.isEmpty { albumName = album }
            }
        }
    }
}
