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
    // Off by default: the interactive scroll transition slides the image,
    // which fights burst comparison. With it off we drive paging by discrete
    // swipe gestures that cut instantly to the next frame — no slide.
    @AppStorage("viewerAnimations") private var animate = false

    func makeCoordinator() -> Coordinator { Coordinator(model: model, index: $index) }

    func makeUIViewController(context: Context) -> UIPageViewController {
        let pvc = UIPageViewController(transitionStyle: .scroll,
                                       navigationOrientation: .horizontal,
                                       options: [.interPageSpacing: 8])
        pvc.dataSource = context.coordinator
        pvc.delegate = context.coordinator
        pvc.view.backgroundColor = .black
        pvc.setViewControllers([context.coordinator.page(index)], direction: .forward, animated: false)
        context.coordinator.installSwipes(on: pvc.view)
        return pvc
    }

    func updateUIViewController(_ pvc: UIPageViewController, context: Context) {
        let zoomed = model.viewerZoomed
        context.coordinator.animate = animate
        context.coordinator.sync(pvc, to: index)
        // Interactive scroll paging: only when animations are on AND we're not
        // zoomed (zoomed, the frame owns drags for panning). With animations
        // off, scroll is locked and the discrete swipe recognizers page
        // instantly instead. Keyboard/filmstrip/decide navigation always works
        // via setViewControllers, independent of the scroll view.
        context.coordinator.pagerScroll(pvc)?.isScrollEnabled = !zoomed && animate
        context.coordinator.setSwipeEnabled(!zoomed && !animate)
    }

    @MainActor
    final class Coordinator: NSObject, UIPageViewControllerDataSource, UIPageViewControllerDelegate {
        private let model: GridModel
        private let index: Binding<Int>

        /// Mirrors PagerView's viewer-animation preference; updated each
        /// updateUIViewController before sync so the transition matches.
        var animate = false

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
        private weak var leftSwipe: UISwipeGestureRecognizer?
        private weak var rightSwipe: UISwipeGestureRecognizer?

        /// UIPageViewController drives paging through a private UIScrollView;
        /// we toggle its `isScrollEnabled` to lock paging while a page is
        /// zoomed. Found once by walking the subviews, then cached.
        func pagerScroll(_ pvc: UIPageViewController) -> UIScrollView? {
            if let s = cachedScroll { return s }
            cachedScroll = pvc.view.subviews.compactMap { $0 as? UIScrollView }.first
            return cachedScroll
        }

        /// Discrete swipe recognizers used when animations are off: a swipe
        /// left/right cuts to the next/previous frame with no slide. They stay
        /// disabled while the interactive scroll pager is doing the work.
        func installSwipes(on view: UIView) {
            let left = UISwipeGestureRecognizer(target: self, action: #selector(swipeNext))
            left.direction = .left
            let right = UISwipeGestureRecognizer(target: self, action: #selector(swipePrev))
            right.direction = .right
            view.addGestureRecognizer(left)
            view.addGestureRecognizer(right)
            leftSwipe = left
            rightSwipe = right
            setSwipeEnabled(false)
        }

        func setSwipeEnabled(_ on: Bool) {
            leftSwipe?.isEnabled = on
            rightSwipe?.isEnabled = on
        }

        @objc private func swipeNext() { hop(+1) }
        @objc private func swipePrev() { hop(-1) }

        /// Instant one-frame hop: moving the binding drives sync() (animated
        /// false, since animations are off whenever these fire) → a clean cut.
        private func hop(_ delta: Int) {
            let target = index.wrappedValue + delta
            guard model.shots.indices.contains(target) else { return }
            index.wrappedValue = target
        }

        /// External index changes (filmstrip tap, keyboard, decide-advance):
        /// animate to the new page unless the pager is already there. Never
        /// fights an in-flight swipe — the delegate reconciles the binding
        /// when the gesture finishes.
        func sync(_ pvc: UIPageViewController, to i: Int) {
            guard !transitioning, model.shots.indices.contains(i),
                  let current = pvc.viewControllers?.first, current.view.tag != i else { return }
            let dir: UIPageViewController.NavigationDirection = i > current.view.tag ? .forward : .reverse
            // Animate the slide only when the preference is on and it's a
            // single-step move; otherwise cut instantly (burst comparison).
            let slide = animate && abs(i - current.view.tag) == 1
            pvc.setViewControllers([page(i)], direction: dir, animated: slide)
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
