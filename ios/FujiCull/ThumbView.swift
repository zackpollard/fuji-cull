import SwiftUI

// ThumbCache is a small in-memory decoded-image cache (Nuke is the planned
// heavier equivalent; NSCache is enough for the simulator/fake corpus).
final class ThumbCache {
    static let shared = ThumbCache()
    private let cache = NSCache<NSString, UIImage>()
    init() { cache.countLimit = 800 }
    func image(for key: String) -> UIImage? { cache.object(forKey: key as NSString) }
    func set(_ img: UIImage, for key: String) { cache.setObject(img, forKey: key as NSString) }
}

// ThumbView loads and displays one grid thumbnail, cached by id+orientation.
// `ready` gates the fetch so we don't hammer /api/thumb with 404s before the
// engine's sweep has produced the file.
struct ThumbView: View {
    let url: URL
    let cacheKey: String
    let ready: Bool
    @State private var image: UIImage?

    var body: some View {
        ZStack {
            if let image {
                Image(uiImage: image).resizable().scaledToFill()
            } else {
                Rectangle().fill(Color.white.opacity(0.04))
                if !ready {
                    Image(systemName: "photo")
                        .foregroundStyle(.white.opacity(0.10))
                        .font(.system(size: 20))
                }
            }
        }
        .task(id: "\(cacheKey)-\(ready)") { await load() }
    }

    private func load() async {
        if image != nil { return }
        if let cached = ThumbCache.shared.image(for: cacheKey) { image = cached; return }
        guard ready else { return }
        // local files (client-side video posters): no HTTP response to check —
        // the status-code guard below silently rejected every file:// load
        if url.isFileURL {
            guard let img = UIImage(contentsOfFile: url.path) else { return }
            ThumbCache.shared.set(img, for: cacheKey)
            image = img
            return
        }
        guard let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let img = UIImage(data: data) else { return }
        ThumbCache.shared.set(img, for: cacheKey)
        image = img
    }
}
