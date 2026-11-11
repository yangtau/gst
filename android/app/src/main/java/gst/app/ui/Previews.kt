package gst.app.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.tooling.preview.Preview
import gst.app.Demo

// Android Studio's previews of the screens, on the made-up network of Demo.

@Composable
private fun Show(demo: Demo, dark: Boolean = false) {
    val model = remember { demo.model() }
    GstTheme(dark = dark, brandColors = true) { GstApp(model, demo.route, demo.machine, demo.addStep) }
}

@Preview(device = "id:pixel_9") @Composable private fun Home() = Show(Demo.Home)
@Preview(device = "id:pixel_9") @Composable private fun HomeDark() = Show(Demo.Home, dark = true)
@Preview(device = "id:pixel_9") @Composable private fun Problems() = Show(Demo.Problems)
@Preview(device = "id:pixel_9") @Composable private fun Welcome() = Show(Demo.Welcome)
@Preview(device = "id:pixel_9") @Composable private fun Off() = Show(Demo.Off)
@Preview(device = "id:pixel_9") @Composable private fun HostKey() = Show(Demo.HostKey)
@Preview(device = "id:pixel_9") @Composable private fun Authorize() = Show(Demo.Authorize)
@Preview(device = "id:pixel_9") @Composable private fun Settings() = Show(Demo.Settings)
