package gst.app.ui

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.WarningAmber
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import gst.app.AppModel
import gst.app.Demo
import gst.app.Pings
import gst.app.R

/** A screen of the app. */
sealed interface Route {
    data object Home : Route
    data class Machine(val id: String) : Route
    data object Add : Route
    data object Settings : Route
    data object Log : Route
}

/**
 * The app: the network, and the screens that open from it, one over
 * another. On a made-up network, it opens on the screen that [demo] is of.
 */
@Composable
fun GstApp(model: AppModel, demo: Demo? = null) {
    // The page, and the color of what is on it.
    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        Screens(model, demo)
    }
}

@Composable
private fun Screens(model: AppModel, demo: Demo?) {
    val state by model.tunnel.collectAsStateWithLifecycle()
    val route = demo?.route ?: Route.Home
    // The screens open, the network first; back closes the last.
    var stack by remember { mutableStateOf(if (route == Route.Home) listOf(route) else listOf(Route.Home, route)) }
    // What shows over the network, which stays out of the way of a screen that opens.
    var sheet by remember { mutableStateOf(demo?.sheet) }
    fun open(next: Route) {
        stack = stack + next
    }
    fun close() {
        stack = stack.dropLast(1)
    }
    val homeScroll = rememberScrollState()
    val failure = model.failure
    if (failure != null) {
        Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            Notice(Icons.Rounded.WarningAmber, stringResource(R.string.cannot_start), failure)
        }
        return
    }
    val spec = MaterialTheme.motionScheme.defaultSpatialSpec<androidx.compose.ui.unit.IntOffset>()
    AnimatedContent(
        stack,
        transitionSpec = {
            val forward = targetState.size > initialState.size
            (slideInHorizontally(spec) { if (forward) it / 4 else -it / 4 } + fadeIn()) togetherWith
                (slideOutHorizontally(spec) { if (forward) -it / 4 else it / 4 } + fadeOut())
        },
        contentKey = { it.last() },
        label = "route",
    ) { shown ->
        when (val top = shown.last()) {
            Route.Home -> HomeScreen(
                model, state, homeScroll, sheet,
                show = { sheet = it },
                openAdd = { open(Route.Add) },
                openSettings = { open(Route.Settings) },
                openMachine = { open(Route.Machine(it)) },
                pings = demo?.pings ?: Pings(),
            )
            is Route.Machine -> MachineScreen(model, top.id, state == gst.app.Tunnel.State.On, ::close)
            Route.Add -> AddMachineScreen(model, close = ::close, initialStep = demo?.addStep ?: AddStep.Form)
            Route.Settings -> SettingsScreen(model, close = ::close, openLog = { open(Route.Log) })
            Route.Log -> LogScreen(model, close = ::close)
        }
    }
}
