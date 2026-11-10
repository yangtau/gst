import XCTest

/// Opens every screen of the app on its made-up network (Demo), checks
/// that the screen shows it, and keeps a picture: as an attachment of the
/// test, and as a file in the directory that GST_SCREENSHOTS names
/// (`TEST_RUNNER_GST_SCREENSHOTS` for xcodebuild), called
/// LANGUAGE-APPEARANCE-SCREEN.png.
final class ScreenTests: XCTestCase {
    /// A screen of Demo, and a text of the made-up network that it shows
    /// in every language.
    private struct Screen {
        let name: String
        let shows: String
    }

    private let screens = [
        Screen(name: "home", shows: "homelab"),
        Screen(name: "problems", shows: "192.0.2.20"),
        Screen(name: "welcome", shows: "iphone"),
        Screen(name: "off", shows: "attic"),
        Screen(name: "host-key", shows: "SHA256:"),
        Screen(name: "authorize", shows: "authorized_keys"),
        Screen(name: "machine", shows: "devbox.gst"),
        Screen(name: "settings", shows: "admin@ssh.example.com"),
    ]

    override func setUp() {
        continueAfterFailure = true
    }

    func testChinese() {
        show(screens, language: "zh-Hans", locale: "zh_CN", appearance: "light")
    }

    func testEnglish() {
        show(screens, language: "en", locale: "en_US", appearance: "light")
    }

    func testDark() {
        let dark = screens.filter { ["home", "problems", "welcome", "machine", "host-key", "settings"].contains($0.name) }
        show(dark, language: "zh-Hans", locale: "zh_CN", appearance: "dark")
    }

    private func show(_ screens: [Screen], language: String, locale: String, appearance: String) {
        for screen in screens {
            let app = XCUIApplication()
            app.launchArguments = [
                "-GstDemo", screen.name, "-GstDemoAppearance", appearance,
                "-AppleLanguages", "(\(language))", "-AppleLocale", locale,
            ]
            app.launch()
            let shown = app.descendants(matching: .any)
                .matching(NSPredicate(format: "label CONTAINS %@", screen.shows))
                .firstMatch
            XCTAssertTrue(shown.waitForExistence(timeout: 30), "\(screen.name) in \(language) does not show \(screen.shows)")
            // A sheet takes a moment to come to rest.
            Thread.sleep(forTimeInterval: 1.5)
            keep(XCUIScreen.main.screenshot(), as: "\(language)-\(appearance)-\(screen.name)")
            app.terminate()
        }
    }

    private func keep(_ screenshot: XCUIScreenshot, as name: String) {
        let attachment = XCTAttachment(screenshot: screenshot)
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
        guard let directory = ProcessInfo.processInfo.environment["GST_SCREENSHOTS"], !directory.isEmpty else { return }
        do {
            let url = URL(fileURLWithPath: directory, isDirectory: true)
            try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
            try screenshot.pngRepresentation.write(to: url.appendingPathComponent(name + ".png"))
        } catch {
            XCTFail("the picture of \(name) cannot be written: \(error.localizedDescription)")
        }
    }
}
