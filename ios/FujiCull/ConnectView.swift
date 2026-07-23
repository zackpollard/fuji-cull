import SwiftUI

// ConnectView mirrors the Android connect screen: engine/discovery status, the
// camera-link diagnostic, hookup guidance, settings/log access, and a live tail
// of the engine log so bring-up is legible without a debugger attached.
struct ConnectView: View {
    @EnvironmentObject var engine: Engine
    @EnvironmentObject var settings: SettingsStore
    @State private var showLog = false
    @State private var showSettings = false

    var body: some View {
        VStack(spacing: 18) {
            Spacer()

            Text("fuji-cull")
                .font(.system(size: 34, weight: .bold, design: .monospaced))
                .foregroundStyle(Color.amber)

            if let err = engine.startError {
                Label(err, systemImage: "xmark.octagon.fill")
                    .foregroundStyle(.red)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
            } else {
                ProgressView().tint(Color.amber)
                Text(engine.status)
                    .font(.system(.body, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
                if engine.shotCount > 0 {
                    Text("\(engine.shotCount) shots")
                        .font(.system(.title3, design: .monospaced))
                }
            }

            Text("set the camera to USB card-reader mode and connect it to the iPad")
                .font(.system(.footnote, design: .monospaced))
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .padding(.horizontal)

            if !engine.cameraStatus.isEmpty {
                Text(engine.cameraStatus)
                    .font(.system(.footnote, design: .monospaced))
                    .foregroundStyle(Color.amber)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
            }
            if engine.mode == .fake {
                Label("FAKE CORPUS — not your card", systemImage: "exclamationmark.triangle.fill")
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(Color.rejectRed)
            }

            HStack(spacing: 18) {
                Button("settings") { showSettings = true }
                Button("log") { showLog = true }
            }
            .font(.system(.footnote, design: .monospaced))
            .foregroundStyle(.secondary)

            LogTailView(text: engine.log)
                .frame(maxHeight: 200)
                .padding(.horizontal)

            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color.appBG.ignoresSafeArea())
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
