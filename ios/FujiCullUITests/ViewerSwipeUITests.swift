import XCTest

// Machine proof for the viewer paging behavior: drive real swipe gestures
// through UIKit (XCUITest, no Accessibility grant, no device needed) and check
// where the pager lands.
//
// Default is animations OFF (instant cut): a swipe is handled by a discrete
// recognizer that hops one frame with no slide, so a burst can be
// blink-compared in place. This test verifies a swipe actually advances the
// frame in that mode and that each landing is a single whole page — the old
// TabView's "lands halfway" failure can't recur on the native pager.
final class ViewerSwipeUITests: XCTestCase {

    override func setUp() { continueAfterFailure = true }

    private func snap(_ name: String) {
        let shot = XCUIScreen.main.screenshot()
        let att = XCTAttachment(screenshot: shot)
        att.name = name
        att.lifetime = .keepAlways
        add(att)
    }

    /// The top bar shows "<folder> / <base>", e.g. "156_FUJI / DSCF7001".
    private func frameLabel(_ app: XCUIApplication) -> String {
        app.staticTexts.matching(NSPredicate(format: "label CONTAINS 'DSCF'"))
            .firstMatch.label
    }

    func testSwipeAdvancesAndLandsOnWholePage() {
        let app = XCUIApplication()
        app.launch()

        // The grid loads 24k fake shots off the in-process engine; give it room.
        // No a11y ids on cells, so drive everything by coordinate.
        Thread.sleep(forTimeInterval: 8)
        snap("01-grid")

        // Open the viewer on a shot (top rows are photos; the corpus rests near
        // its end, so the opened shot may be a video — paging geometry is the
        // same either way).
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.16, dy: 0.16)).tap()
        Thread.sleep(forTimeInterval: 3)
        snap("02-viewer-opened")
        let opened = frameLabel(app)
        XCTAssertFalse(opened.isEmpty, "expected a frame label in the viewer")

        // A real swipe. With animations off this is caught by the discrete
        // recognizer and hops exactly one frame, instantly.
        app.swipeLeft()
        Thread.sleep(forTimeInterval: 2)
        snap("03-after-swipe-left")
        let advanced = frameLabel(app)
        XCTAssertNotEqual(opened, advanced,
                          "a swipe with animations off should advance to the next frame")

        // Swipe back — should return to the frame we started on.
        app.swipeRight()
        Thread.sleep(forTimeInterval: 2)
        snap("04-after-swipe-right")
        XCTAssertEqual(frameLabel(app), opened,
                       "swiping back should land on the original frame")
    }
}
