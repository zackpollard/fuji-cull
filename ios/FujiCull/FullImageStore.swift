import UIKit

// FullImageStore is the decoded-image cache for the full-frame viewer — the
// full-image analogue of ThumbCache. Without it, every page the pager creates
// re-fetched and re-decoded its JPEG through an async .task, so an instant
// (un-animated) swap showed the new page's black background for a frame or two
// before the image landed: the "black flash" when flicking a burst.
//
// Two jobs: hand back an already-decoded image synchronously so a new
// ZoomableImage can render it on its very first frame, and prefetch the
// neighbours (fetch + force-decode off the main thread) so a forward flick is
// ready before the finger lifts.
final class FullImageStore {
    static let shared = FullImageStore()

    private let cache = NSCache<NSString, UIImage>()
    private let lock = NSLock()
    private var inflight = Set<String>()

    init() {
        // Full-frame JPEGs decode large (~100 MB each at 26 MP), so budget by
        // bytes and keep the window small; NSCache also drops under pressure.
        cache.totalCostLimit = 448 << 20
    }

    func image(for url: URL) -> UIImage? {
        cache.object(forKey: url.absoluteString as NSString)
    }

    /// Cache an image the viewer just decoded on-demand, so paging back to it
    /// (or re-showing after a page rebuild) is instant.
    func remember(_ img: UIImage, for url: URL) {
        let cost = Int(img.size.width * img.size.height * img.scale * img.scale * 4)
        cache.setObject(img, forKey: url.absoluteString as NSString, cost: cost)
    }

    /// Fetch + force-decode `url` on a background task if it isn't already
    /// cached or in flight, so the next flick lands on a ready bitmap.
    func prefetch(_ url: URL) {
        let key = url.absoluteString
        if cache.object(forKey: key as NSString) != nil { return }
        lock.lock()
        if inflight.contains(key) { lock.unlock(); return }
        inflight.insert(key)
        lock.unlock()

        Task.detached(priority: .utility) { [weak self] in
            defer {
                self?.lock.lock()
                self?.inflight.remove(key)
                self?.lock.unlock()
            }
            guard let (data, resp) = try? await URLSession.shared.data(from: url),
                  (resp as? HTTPURLResponse)?.statusCode == 200,
                  let raw = UIImage(data: data),
                  // byPreparingForDisplay decodes now, off the main thread, so
                  // the first draw doesn't stall (a stall reads as a flash too)
                  let ready = await raw.byPreparingForDisplay() else { return }
            self?.remember(ready, for: url)
        }
    }
}
