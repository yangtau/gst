package gst.app

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.repeatOnLifecycle
import gst.app.ui.GstApp
import gst.app.ui.GstTheme
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.delay

/** Opens gst, and then shows the network. */
class MainActivity : ComponentActivity() {
    /** The answer to the system's question whether gst may be a VPN, while it is asked. */
    private var consent: CompletableDeferred<Boolean>? = null
    private val askConsent = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) {
        consent?.complete(it.resultCode == RESULT_OK)
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val demo = if (BuildConfig.DEBUG) Demo.from(intent) else null
        val demoDark = intent.getStringExtra("GstDemoAppearance")?.let { it == "dark" }
        val model = demo?.model() ?: (application as GstApplication).model
        model.askConsent = { request: Intent ->
            CompletableDeferred<Boolean>().also { consent = it; askConsent.launch(request) }.await()
        }
        setContent {
            GstTheme(dark = demoDark ?: androidx.compose.foundation.isSystemInDarkTheme(), brandColors = demo != null) {
                val state by model.tunnel.collectAsStateWithLifecycle()
                var isStarted by remember { mutableStateOf(false) }
                LaunchedEffect(Unit) {
                    model.start()
                    isStarted = true
                }
                // What the daemon reports changes by itself, as links connect
                // and machines come and go, so the app asks while it is in view.
                LaunchedEffect(isStarted, state) {
                    if (!isStarted) return@LaunchedEffect
                    repeatOnLifecycle(Lifecycle.State.RESUMED) {
                        while (true) {
                            model.refresh()
                            delay(2000)
                        }
                    }
                }
                if (demo != null) {
                    GstApp(model, demo.route, demo.machine, demo.addStep)
                } else {
                    GstApp(model)
                }
            }
        }
    }
}
