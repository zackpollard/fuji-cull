import SwiftUI

// ConnectView mirrors the Android connect screen: engine/discovery status, the
// camera-link diagnostic, hookup guidance, settings/log access, and a live tail
// of the engine log so bring-up is legible without a debugger attached.
struct ConnectView: View {
    @EnvironmentObject var engine: Engine
    @EnvironmentObject var settings: SettingsStore
    @State private var showLog = false
    @State private var showSettings = false

    /// The bring-up phases, derived from live status strings — real
    /// telemetry, no fake spinners (design: "make the waits feel engineered").
    private var phase: Int {
        let s = (engine.status + " " + engine.cameraStatus).lowercased()
        if engine.ready { return 4 }
        if s.contains("card index") || s.contains("reading camera index") { return 3 }
        if s.contains("indexing") || s.contains("catalog") { return 2 }
        if s.contains("opening") || s.contains("connecting") || s.contains("session") { return 1 }
        return 0
    }

    private let phaseNames = ["find camera", "open session", "index card", "read card index"]

    var body: some View {
        VStack(spacing: DS.s4) {
            Spacer()

            Text("fuji-cull")
                .font(DS.counter(34))
                .foregroundStyle(DS.amber)

            if let err = engine.startError {
                Label(err, systemImage: "xmark.octagon.fill")
                    .font(DS.body())
                    .foregroundStyle(DS.reject)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
            } else {
                // numbered phase telemetry
                VStack(alignment: .leading, spacing: DS.s2) {
                    ForEach(Array(phaseNames.enumerated()), id: \.offset) { i, name in
                        HStack(spacing: DS.s2) {
                            if i < phase {
                                Image(systemName: "checkmark").font(.system(size: 12, weight: .bold))
                                    .foregroundStyle(DS.keep)
                            } else if i == phase {
                                ProgressView().controlSize(.small).tint(DS.amber)
                            } else {
                                Text("\(i + 1)").font(DS.micro())
                                    .foregroundStyle(DS.text3)
                                    .frame(width: 14)
                            }
                            Text(name.uppercased())
                                .font(DS.label(13))
                                .foregroundStyle(i <= phase ? DS.text : DS.text3)
                        }
                    }
                }
                .padding(DS.s4)
                .background(DS.tile, in: RoundedRectangle(cornerRadius: DS.rSheet))

                Text(engine.cameraStatus.isEmpty ? engine.status : engine.cameraStatus)
                    .font(DS.mono(14))
                    .foregroundStyle(DS.amber)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
            }

            Text("set the camera to USB card-reader mode and connect it to the iPad")
                .font(DS.body(13))
                .foregroundStyle(DS.text2)
                .multilineTextAlignment(.center)
                .padding(.horizontal)

            if engine.mode == .fake {
                Label("FAKE CORPUS — not your card", systemImage: "exclamationmark.triangle.fill")
                    .font(DS.label(12))
                    .foregroundStyle(DS.reject)
            }

            HStack(spacing: DS.s4) {
                Button("SETTINGS") { showSettings = true }
                Button("LOG") { showLog = true }
            }
            .font(DS.label(12))
            .foregroundStyle(DS.text2)

            LogTailView(text: engine.log)
                .frame(maxHeight: 200)
                .padding(.horizontal)

            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(DS.bg.ignoresSafeArea())
        .sheet(isPresented: $showLog) { LogSheet(engine: engine) }
        .sheet(isPresented: $showSettings) { SettingsView() }
    }
}

// LogTailView renders the engine log, auto-scrolled to the newest line.
// Callers must keep `text` modest (a few hundred lines at most): a huge
// selectable Text re-laid-out on every change once starved the main thread
// into a permanent freeze.
struct LogTailView: View {
    let text: String
    var selectable = false

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                Group {
                    if selectable {
                        logText.textSelection(.enabled)
                    } else {
                        logText
                    }
                }
                .id("logbottom")
            }
            .onChange(of: text) { _ in
                proxy.scrollTo("logbottom", anchor: .bottom)
            }
        }
        .background(Color.black.opacity(0.35))
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    private var logText: some View {
        Text(text.isEmpty ? "…" : text)
            .font(.system(size: 10, design: .monospaced))
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

// LogSheet is the full diagnostics log with share (the engine persists it to a
// file, so sharing gives the complete history, not just the in-memory ring).
struct LogSheet: View {
    @ObservedObject var engine: Engine
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""

    var body: some View {
        NavigationStack {
            // render a bounded tail (full text still shared via ShareLink) —
            // an unbounded selectable Text was a main-thread killer
            LogTailView(text: String(text.suffix(40_000)), selectable: true)
                .padding(8)
                .navigationTitle("engine log :\(engine.port)")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .topBarLeading) {
                        ShareLink(item: text) { Image(systemName: "square.and.arrow.up") }
                    }
                    ToolbarItem(placement: .topBarTrailing) { Button("Done") { dismiss() } }
                }
                .task {
                    while !Task.isCancelled {
                        let t = engine.fullLog()
                        if t != text { text = t }
                        try? await Task.sleep(nanoseconds: 2_000_000_000)
                    }
                }
        }
    }
}

extension Color {
    static let amber = Color(red: 1.0, green: 0.70, blue: 0.18)
    static let keepGreen = Color(red: 0.22, green: 0.84, blue: 0.48)
    static let rejectRed = Color(red: 1.0, green: 0.35, blue: 0.24)
    static let appBG = Color(red: 0.043, green: 0.047, blue: 0.043)
}
