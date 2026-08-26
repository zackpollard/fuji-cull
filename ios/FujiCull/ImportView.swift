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
    @State private var reimport = false
    @State private var albumName: String = ""

    private var keepers: Int { model.counts["keep"] ?? 0 }
    private var status: ImportStatus? { model.importStatus }
    private var running: Bool { status?.running ?? false }

    var body: some View {
        // What this run would actually send, once the engine has told us.
        let pending = model.pendingImport
        let importCount = reimport ? keepers : (pending?.shots ?? keepers)
        let importLabel = "Import \(importCount) keeper\(importCount == 1 ? "" : "s")"
        return NavigationStack {
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
                    // "keep" is a queue: shots already imported drop out, so a
                    // finished event is not sent again — nor filed into the
                    // next event's album.
                    if let p = model.pendingImport, p.imported > 0 {
                        Toggle("Re-import already imported", isOn: $reimport)
                        Text("\(p.imported) already imported — skipped")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    Button {
                        model.startImport(dest: dest, album: albumName, reimport: reimport)
                    } label: {
                        Label(importLabel, systemImage: "square.and.arrow.down")
                    }
                    .disabled(importCount == 0 || running)
                } footer: {
                    Text("Copies kept shots to the destination. Rejects and undecided are left on the card.")
                }

                if let s = status, s.running || s.phase == "done" || s.phase == "error" {
                    // One lane per stage, all drawn every tick. Copy, hash and
                    // upload overlap, so no lane is "the current step" — the
                    // four-phase checklist this replaces could only ever show
                    // one number, and the one it showed was the camera count.
                    Section(progressTitle(s)) {
                        StageLaneRow(name: "CAMERA", color: DS.keep, stage: s.camera,
                                     counter: cameraCounter(s.camera))
                        StageLaneRow(name: "UPLOAD", color: DS.immich, stage: s.upload,
                                     counter: uploadCounter(s.upload))
                        StageLaneRow(name: "STACK", color: DS.buffered, stage: s.stack,
                                     counter: stackCounter(s.stack))
                        // Verify is one bulk checksum query lasting a second or
                        // two: a status line, not a bar anyone can watch.
                        if let v = s.verify, v.state != "n/a" {
                            HStack {
                                Text("VERIFY").font(DS.label(13))
                                    .foregroundStyle(v.state == "pending" ? DS.text3 : DS.text2)
                                Spacer()
                                Text(v.state == "pending"
                                     ? "after the last upload"
                                     : "\(v.files) / \(v.filesTotal) on server")
                                    .font(DS.micro())
                                    .foregroundStyle(v.state == "pending" ? DS.text3 : DS.text2)
                            }
                        }
                        if let ft = s.fileTotal, ft > 0 {
                            LaneBar(name: s.file ?? "", nameColor: DS.text,
                                    counter: fileCounter(s), color: DS.amber,
                                    fraction: Double(s.fileSent ?? 0) / Double(ft),
                                    height: 3)
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

// MARK: - progress lanes

/// Binary units labelled GB/MB, matching humanBytes on the other clients.
private func humanBytes(_ n: Int64) -> String {
    if n >= 1 << 30 { return String(format: "%.2f GB", Double(n) / Double(1 << 30)) }
    if n >= 1 << 20 { return String(format: "%.1f MB", Double(n) / Double(1 << 20)) }
    return "\(n / 1024) KB"
}

private func humanRate(_ bps: Double) -> String {
    if bps >= Double(1 << 20) { return String(format: "%.1f MB/s", bps / Double(1 << 20)) }
    if bps >= Double(1 << 10) { return String(format: "%.0f KB/s", bps / Double(1 << 10)) }
    return String(format: "%.0f B/s", bps)
}

private func bits(_ parts: [String?]) -> String {
    parts.compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
}

private func progressTitle(_ s: ImportStatus) -> String {
    var t = "Progress"
    if let e = s.elapsedSec, e > 0 { t += String(format: " · %d:%02d", e / 60, e % 60) }
    if !s.running { t += " — finished" }
    return t
}

/// Cached files are named here rather than shaded into the bar. Half a JPG+RAF
/// import lands in the first instant off the browse cache; a colour band for
/// that needed a legend, "200 cached" does not.
private func cameraCounter(_ st: ImportStage?) -> String {
    guard let st else { return "" }
    return bits([
        "\(st.files) / \(st.filesTotal)",
        (st.bytesTotal ?? 0) > 0 ? humanBytes(st.bytes ?? 0) : nil,
        (st.cached ?? 0) > 0 ? "\(st.cached!) cached" : nil,
        (st.rate ?? 0) > 0 ? humanRate(st.rate!) : nil,
    ])
}

private func uploadCounter(_ st: ImportStage?) -> String {
    guard let st else { return "" }
    return bits([
        "\(st.files) / \(st.filesTotal)",
        (st.bytesTotal ?? 0) > 0 ? humanBytes(st.bytes ?? 0) : nil,
        (st.rate ?? 0) > 0 ? humanRate(st.rate!) : nil,
        (st.failed ?? 0) > 0 ? "\(st.failed!) failed" : nil,
    ])
}

private func stackCounter(_ st: ImportStage?) -> String {
    guard let st else { return "" }
    return bits([
        "\(st.files) / \(st.filesTotal) pairs",
        (st.rate ?? 0) > 0 ? String(format: "%.1f pairs/s", st.rate!) : nil,
        (st.failed ?? 0) > 0 ? "\(st.failed!) failed" : nil,
    ])
}

private func fileCounter(_ s: ImportStatus) -> String {
    bits([
        humanBytes(s.fileSent ?? 0) + " / " + humanBytes(s.fileTotal ?? 0),
        (s.rateBps ?? 0) > 0 ? humanRate(s.rateBps!) : nil,
    ])
}

/// A stage lane: label and counter on one line, bar full width beneath. Bars
/// are byte-denominated wherever bytes are known — a file count cannot tell a
/// 25 MB JPEG from a 62 MB RAF, and that gap is most of a JPG+RAF import.
private struct StageLaneRow: View {
    let name: String
    let color: Color
    let stage: ImportStage?
    let counter: String

    var body: some View {
        if let st = stage, st.state != "n/a" {
            let pending = st.state == "pending"
            let den = (st.bytesTotal ?? 0) > 0 ? Double(st.bytesTotal!) : Double(st.filesTotal)
            let num = (st.bytesTotal ?? 0) > 0 ? Double(st.bytes ?? 0) : Double(st.files)
            LaneBar(name: name, nameColor: pending ? DS.text3 : color,
                    counter: counter, color: pending ? DS.text3 : color,
                    fraction: den > 0 ? num / den : 0, height: 4,
                    counterColor: pending ? DS.text3 : DS.text2)
        }
    }
}

private struct LaneBar: View {
    let name: String
    let nameColor: Color
    let counter: String
    let color: Color
    let fraction: Double
    let height: CGFloat
    var counterColor: Color = DS.text2

    var body: some View {
        VStack(alignment: .leading, spacing: DS.s2) {
            HStack {
                Text(name).font(DS.label(13)).foregroundStyle(nameColor)
                Spacer()
                Text(counter).font(DS.micro()).foregroundStyle(counterColor)
            }
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Rectangle().fill(DS.line)
                    Rectangle().fill(color)
                        .frame(width: geo.size.width * CGFloat(min(max(fraction, 0), 1)))
                }
            }
            .frame(height: height)
            .clipShape(RoundedRectangle(cornerRadius: 2))
        }
    }
}
