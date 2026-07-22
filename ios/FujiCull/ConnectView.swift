import SwiftUI

// ConnectView mirrors the Android connect screen: engine/discovery status,
// shot count once indexed, the loopback port, and a live tail of the engine
// log so bring-up is legible without a debugger attached.
struct ConnectView: View {
    @EnvironmentObject var engine: Engine

    var body: some View {
        VStack(spacing: 20) {
            Spacer()

            Text("fuji-cull")
                .font(.system(size: 34, weight: .bold, design: .monospaced))
                .foregroundStyle(Color(red: 1.0, green: 0.70, blue: 0.18))

            if let err = engine.startError {
                Label(err, systemImage: "xmark.octagon.fill")
                    .foregroundStyle(.red)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
            } else {
                ProgressView()
                    .tint(.white)
                Text(engine.status)
                    .font(.system(.body, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal)
                if engine.shotCount > 0 {
                    Text("\(engine.shotCount) shots")
                        .font(.system(.title3, design: .monospaced))
                        .foregroundStyle(.primary)
                }
            }

            if engine.port > 0 {
                Text("engine :\(engine.port)")
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.tertiary)
            }

            LogTailView(text: engine.log)
                .frame(maxHeight: 220)
                .padding(.horizontal)

            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(red: 0.043, green: 0.047, blue: 0.043).ignoresSafeArea())
    }
}

// LogTailView renders the last engine-log lines, auto-scrolled to the newest.
struct LogTailView: View {
    let text: String

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                Text(text.isEmpty ? "…" : text)
                    .font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .textSelection(.enabled)
                    .id("logbottom")
            }
            .onChange(of: text) { _ in
                withAnimation { proxy.scrollTo("logbottom", anchor: .bottom) }
            }
        }
        .background(Color.black.opacity(0.35))
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }
}

// HomeView is a temporary landing shown once discovery completes — Phase 2
// replaces it with the grid/viewer.
struct HomeView: View {
    @EnvironmentObject var engine: Engine

    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: "checkmark.seal.fill")
                .font(.system(size: 56))
                .foregroundStyle(Color(red: 0.22, green: 0.84, blue: 0.48))
            Text("engine ready")
                .font(.system(.title, design: .monospaced))
            Text("\(engine.shotCount) shots · :\(engine.port)")
                .font(.system(.body, design: .monospaced))
                .foregroundStyle(.secondary)
            Text("grid & viewer — Phase 2")
                .font(.system(.footnote, design: .monospaced))
                .foregroundStyle(.tertiary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(red: 0.043, green: 0.047, blue: 0.043).ignoresSafeArea())
    }
}
