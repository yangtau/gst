package gst.app.ui

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import gst.app.AppModel
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
 * another. A demo opens it on [route] (a machine's page starting with
 * [pings]), and on [addStep] of adding a machine.
 */
@Composable
fun GstApp(model: AppModel, route: Route = Route.Home, addStep: AddStep = AddStep.Form, pings: Pings = Pings()) {
    // The page, and the color of what is on it.
    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        Screens(model, route, addStep, pings)
    }
}

@Composable
private fun Screens(model: AppModel, route: Route, addStep: AddStep, pings: Pings) {
    val state by model.tunnel.collectAsStateWithLifecycle()
    // The screens open, the network first; back closes the last.
    var stack by remember { mutableStateOf(if (route == Route.Home) listOf(route) else listOf(Route.Home, route)) }
    fun open(next: Route) {
        stack = stack + next
    }
    fun close() {
        stack = stack.dropLast(1)
    }
    val homeScroll = rememberScrollState()
    val failure = model.failure
    if (failure != null) {
        Column(
            Modifier.fillMaxSize().padding(32.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Icon(Icons.Rounded.Warning, contentDescription = null, tint = MaterialTheme.colorScheme.error, modifier = Modifier.size(48.dp))
            Text(stringResource(R.string.cannot_start), style = MaterialTheme.typography.headlineSmall)
            Text(failure, textAlign = TextAlign.Center, color = MaterialTheme.colorScheme.onSurfaceVariant)
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
                model, state, homeScroll,
                openAdd = { open(Route.Add) },
                openSettings = { open(Route.Settings) },
                openMachine = { open(Route.Machine(it)) },
            )
            is Route.Machine -> MachineScreen(model, top.id, state == gst.app.Tunnel.State.On, if (top == route) pings else Pings(), ::close)
            Route.Add -> AddMachineScreen(model, close = ::close, initialStep = addStep)
            Route.Settings -> SettingsScreen(model, close = ::close, openLog = { open(Route.Log) })
            Route.Log -> LogScreen(model, close = ::close)
        }
    }
}
