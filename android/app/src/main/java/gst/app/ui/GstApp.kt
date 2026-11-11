package gst.app.ui

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.SheetValue
import androidx.compose.material3.rememberBottomSheetState
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
import gst.app.R

/** A screen of the app, besides a machine's, which opens over the network. */
enum class Route { Home, Add, Settings, Log }

/** The app: the network, and the screens that open from it. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun GstApp(model: AppModel, route: Route = Route.Home, machine: String? = null, addStep: AddStep = AddStep.Form) {
    val state by model.tunnel.collectAsStateWithLifecycle()
    var current by remember { mutableStateOf(route) }
    var open by remember { mutableStateOf(machine) }
    val failure = model.failure
    if (failure != null) {
        Column(
            Modifier.fillMaxSize().background(MaterialTheme.colorScheme.surface).padding(32.dp),
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
        current,
        transitionSpec = {
            val forward = targetState.ordinal > initialState.ordinal
            (slideInHorizontally(spec) { if (forward) it / 4 else -it / 4 } + fadeIn()) togetherWith
                (slideOutHorizontally(spec) { if (forward) -it / 4 else it / 4 } + fadeOut())
        },
        label = "route",
    ) { shown ->
        when (shown) {
            Route.Home -> HomeScreen(
                model, state,
                openAdd = { current = Route.Add },
                openSettings = { current = Route.Settings },
                openMachine = { open = it },
            )
            Route.Add -> AddMachineScreen(model, close = { current = Route.Home }, initialStep = addStep)
            Route.Settings -> SettingsScreen(model, close = { current = Route.Home }, openLog = { current = Route.Log })
            Route.Log -> LogScreen(model, close = { current = Route.Settings })
        }
    }
    open?.let { id ->
        // Half the screen first, as a glance; a demo shows it all.
        val values = if (machine != null) setOf(SheetValue.Hidden, SheetValue.Expanded) else SheetValue.entries.toSet()
        MachineSheet(model, id, state == gst.app.Tunnel.State.On, rememberBottomSheetState(SheetValue.Hidden, values)) { open = null }
    }
}
