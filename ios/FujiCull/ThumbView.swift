import SwiftUI

// ThumbCache is the in-memory decoded-image cache, budgeted by bytes rather
// than a small fixed count: scrubbing a 24k catalog evicted everything under
// the old 800-entry cap, which is half of why photo tiles "loaded in" while
// the 341 resident video posters appeared instantly.
final class ThumbCache {
    static let shared = ThumbCache()
    private let cache = NSCache<NSString, UIImage>()
    init() {
        cache.countLimit = 4000
        cache.totalCostLimit = 192 << 20
    }
    func image(for key: String) -> UIImage? { cache.object(forKey: key as NSString) }
    func set(_ img: UIImage, for key: String) {
        let cost = Int(img.size.width * img.size.height * img.scale * img.scale * 4)
        cache.setObject(img, forKey: key as NSString, cost: cost)
    }

    /// Generous HTTP cache for the loopback thumb responses (the engine marks
    /// them immutable). Call once at startup.
    static func bumpURLCache() {
        URLCache.shared = URLCache(memoryCapacity: 64 << 20, diskCapacity: 256 << 20)
    }
}

// ThumbView loads and displays one grid thumbnail, cached by id+orientation.
// `ready` gates the fetch so we don't hammer /api/thumb with 404s before the
// engine's sweep has produced the file. EXIF orientation is applied here as
// display metadata (UIImage.Orientation — free, composited by the GPU); the
// engine's per-request pixel rotation was the other half of slow photo tiles.
struct ThumbView: View {
    let url: URL
    let cacheKey: String
    let ready: Bool
    var exifOrient: Int = 0
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
        // a status-code guard would silently reject every file:// load
        if url.isFileURL {
            guard let img = UIImage(contentsOfFile: url.path) else { return }
            ThumbCache.shared.set(img, for: cacheKey)
            image = img
            return
        }
        guard let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let raw = UIImage(data: data) else { return }
        let img: UIImage
        if exifOrient > 1, let cg = raw.cgImage {
            img = UIImage(cgImage: cg, scale: raw.scale, orientation: Self.uiOrientation(exifOrient))
        } else {
            img = raw
        }
        ThumbCache.shared.set(img, for: cacheKey)
        image = img
    }

    /// EXIF orientation value (1-8) to UIImage display orientation.
    static func uiOrientation(_ exif: Int) -> UIImage.Orientation {
        switch exif {
        case 2: return .upMirrored
        case 3: return .down
        case 4: return .downMirrored
        case 5: return .leftMirrored
        case 6: return .right
        case 7: return .rightMirrored
        case 8: return .left
        default: return .up
        }
    }
}
