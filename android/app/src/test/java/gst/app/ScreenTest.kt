package gst.app

import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onRoot
import com.github.takahirom.roborazzi.captureScreenRoboImage
import gst.app.ui.GstApp
import gst.app.ui.GstTheme
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.ParameterizedRobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode

/**
 * Opens every screen of the app on its made-up network (Demo), checks
 * that the screen shows it, and keeps a picture of it in
 * app/build/screenshots, called LANGUAGE-APPEARANCE-SCREEN.png.
 */
@RunWith(ParameterizedRobolectricTestRunner::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE)
@Config(sdk = [36], qualifiers = "w411dp-h914dp-xxhdpi")
class ScreenTest(private val demo: Demo, private val language: String, private val dark: Boolean) {
    companion object {
        /** A text of the made-up network that each screen shows in every language. */
        private val shows = mapOf(
            Demo.Home to "homelab",
            Demo.Problems to "192.0.2.20",
            Demo.Welcome to "pixel",
            Demo.Off to "attic",
            Demo.HostKey to "SHA256:",
            Demo.Authorize to "authorized_keys",
            Demo.Machine to "devbox.gst",
            Demo.Settings to "admin@ssh.example.com",
        )

        private val darkScreens = setOf(Demo.Home, Demo.Problems, Demo.Welcome, Demo.Machine, Demo.HostKey, Demo.Settings)

        @JvmStatic
        @ParameterizedRobolectricTestRunner.Parameters(name = "{1}-{2}-{0}")
        fun screens(): List<Array<Any>> =
            Demo.entries.map { arrayOf<Any>(it, "en", false) } +
                Demo.entries.map { arrayOf<Any>(it, "zh-Hans", false) } +
                Demo.entries.filter { it in darkScreens }.map { arrayOf<Any>(it, "zh-Hans", true) }
    }

    @get:Rule
    val compose = createComposeRule()

    @Test
    fun show() {
        RuntimeEnvironment.setQualifiers(if (language == "en") "+en-rUS" else "+b+zh+Hans")
        RuntimeEnvironment.setQualifiers(if (dark) "+night" else "+notnight")
        // What the system formats, such as "3 hours ago", follows the default locale.
        java.util.Locale.setDefault(if (language == "en") java.util.Locale.US else java.util.Locale.SIMPLIFIED_CHINESE)
        val model = demo.model()
        compose.setContent {
            GstTheme(dark = dark, brandColors = true) {
                GstApp(model, demo.route, demo.machine, demo.addStep)
            }
        }
        compose.waitForIdle()
        // A sheet takes a moment to come to rest.
        compose.mainClock.advanceTimeBy(2000)
        val text = shows.getValue(demo)
        assertTrue(
            "${demo.screen} in $language does not show $text",
            compose.onAllNodesWithText(text, substring = true, useUnmergedTree = true).fetchSemanticsNodes().isNotEmpty(),
        )
        val appearance = if (dark) "dark" else "light"
        captureScreenRoboImage("${System.getProperty("roborazzi.output.dir")}/$language-$appearance-${demo.screen}.png")
    }
}
