import XCTest

// Machine proof for the "swipe lands halfway between images" report: drive a
// real finger-swipe through UIKit's own gesture pipeline (XCUITest, no
// Accessibility grant needed) and capture where the pager actually settles.
// A native UIPageViewController can only rest on a page boundary — this test
// makes that visible instead of arguing it.
final class ViewerSwipeUITests: XCTestCase {

    override func setUp() { continueAfterFailure = true }

    private func snap(_ app: XCUIApplication, _ name: String) {
        let shot = XCUIScreen.main.screenshot()
        let att = XCTAttachment(screenshot: shot)
        att.name = name
        att.lifetime = .keepAlways
        add(att)
    }

    func testSwipeLandsOnWholePage() {
        let app = XCUIApplication()
        app.launch()

        // The grid loads 24k fake shots off the in-process engine; give it room.
        // We don't have a11y ids on cells, so drive everything by coordinate.
        Thread.sleep(forTimeInterval: 8)
        snap(app, "01-grid")

        // Open the viewer on a still image — the top rows are all photos
        // (videos sit at the bottom of the corpus).
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.16, dy: 0.16)).tap()
        Thread.sleep(forTimeInterval: 3) // let /api/image land
        snap(app, "02-viewer-opened")

        // A real, fast horizontal swipe across the pager's center band.
        for i in 1...3 {
            let start = app.coordinate(withNormalizedOffset: CGVector(dx: 0.85, dy: 0.45))
            let end = app.coordinate(withNormalizedOffset: CGVector(dx: 0.15, dy: 0.45))
            start.press(forDuration: 0.05, thenDragTo: end)
            Thread.sleep(forTimeInterval: 2) // let it settle
            snap(app, "03-after-swipe-\(i)")
        }
    }
}
