import SwiftUI
import UIKit

// PagerView is the viewer's horizontal pager: UIPageViewController under
// SwiftUI. The previous windowed TabView re-centered its ForEach window when
// the selection changed — mutating the page set while a swipe settled, which
// left the pager stranded halfway between pages. UIPageViewController
// virtualizes natively: neighbors are dequeued on demand, nothing shifts
// under an in-flight gesture, and snapping is UIKit's own.
struct PagerView: UIViewControllerRepresentable {
    @ObservedObject var model: GridModel
    @Binding var index: Int

    func makeCoordinator() -> Coordinator { Coordinator(model: model, index: $index) }

    func makeUIViewController(context: Context) -> UIPageViewController {
        let pvc = UIPageViewController(transitionStyle: .scroll,
                                       navigationOrientation: .horizontal,
                                       options: [.interPageSpacing: 8])
        pvc.dataSource = context.coordinator
        pvc.delegate = context.coordinator
        pvc.view.backgroundColor = .black
        pvc.setViewControllers([context.coordinator.page(index)], direction: .forward, animated: false)
        return pvc
    }

    func updateUIViewController(_ pvc: UIPageViewController, context: Context) {
        context.coordinator.sync(pvc, to: index)
        // Zoomed in, the frame owns drags: lock the pager's own scroll so a
        // pan can't page. Keyboard/filmstrip/decide navigation still works —
        // those call setViewControllers directly, not the scroll view.
        context.coordinator.pagerScroll(pvc)?.isScrollEnabled = !model.viewerZoomed
    }

    @MainActor
    final class Coordinator: NSObject, UIPageViewControllerDataSource, UIPageViewControllerDelegate {
        private let model: GridModel
        private let index: Binding<Int>

        init(model: GridModel, index: Binding<Int>) {
            self.model = model
            self.index = index
        }

        /// One page. The content observes the model, so `active` (drives
        /// video playback) flips on its own when the current index moves.
        func page(_ i: Int) -> UIViewController {
            let vc = UIHostingController(rootView: PageContent(model: model, i: i))
            vc.view.backgroundColor = .black
            vc.view.tag = i
            return vc
        }

        private var transitioning = false
        private weak var cachedScroll: UIScrollView?

        /// UIPageViewController drives paging through a private UIScrollView;
        /// we toggle its `isScrollEnabled` to lock paging while a page is
        /// zoomed. Found once by walking the subviews, then cached.
        func pagerScroll(_ pvc: UIPageViewController) -> UIScrollView? {
            if let s = cachedScroll { return s }
            cachedScroll = pvc.view.subviews.compactMap { $0 as? UIScrollView }.first
            return cachedScroll
        }

        /// External index changes (filmstrip tap, keyboard, decide-advance):
        /// animate to the new page unless the pager is already there. Never
        /// fights an in-flight swipe — the delegate reconciles the binding
        /// when the gesture finishes.
        func sync(_ pvc: UIPageViewController, to i: Int) {
            guard !transitioning, model.shots.indices.contains(i),
                  let current = pvc.viewControllers?.first, current.view.tag != i else { return }
            let dir: UIPageViewController.NavigationDirection = i > current.view.tag ? .forward : .reverse
            let animate = abs(i - current.view.tag) == 1
            pvc.setViewControllers([page(i)], direction: dir, animated: animate)
        }

        // MARK: data source

        func pageViewController(_ pvc: UIPageViewController,
                                viewControllerBefore vc: UIViewController) -> UIViewController? {
            let i = vc.view.tag - 1
            return i >= 0 ? page(i) : nil
        }

        func pageViewController(_ pvc: UIPageViewController,
                                viewControllerAfter vc: UIViewController) -> UIViewController? {
            let i = vc.view.tag + 1
            return i < model.shots.count ? page(i) : nil
        }

        // MARK: delegate

        func pageViewController(_ pvc: UIPageViewController,
                                willTransitionTo pending: [UIViewController]) {
            transitioning = true
        }

        func pageViewController(_ pvc: UIPageViewController, didFinishAnimating finished: Bool,
                                previousViewControllers: [UIViewController],
                                transitionCompleted completed: Bool) {
            transitioning = false
            guard completed, let vc = pvc.viewControllers?.first else { return }
            if index.wrappedValue != vc.view.tag { index.wrappedValue = vc.view.tag }
        }
    }
}

// PageContent renders one shot, observing the model so the video page's
// `active` state follows the current index without rebuilding the page.
private struct PageContent: View {
    @ObservedObject var model: GridModel
    let i: Int

    var body: some View {
        if model.shots.indices.contains(i) {
            let s = model.shots[i]
            if s.kind == "video" {
                VideoFrame(model: model, shot: s, active: model.viewerIndex == i)
            } else {
                ZoomableImage(url: model.imageURL(s.id), model: model)
            }
        } else {
            Color.black
        }
    }
}
