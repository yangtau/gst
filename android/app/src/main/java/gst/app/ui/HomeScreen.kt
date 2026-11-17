package gst.app.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.expandVertically
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.shrinkVertically
import androidx.compose.foundation.ScrollState
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Hub
import androidx.compose.material.icons.outlined.Key
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Terminal
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.Icon
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import gst.app.AppModel
import gst.app.Machine
import gst.app.R
import gst.app.Tunnel
import kotlinx.coroutines.launch

/** The height of the bar of glass that floats over the bottom of the network. */
private val ControlBarHeight = 68.dp

/**
 * The network: its machines, this one first, each with whether packets
 * reach it, above them, for someone who asks for it, the map of the links,
 * and floating over them the button that turns gst on. [scroll] is kept
 * while a screen opens over it.
 */
@Composable
fun HomeScreen(
    model: AppModel,
    state: Tunnel.State,
    scroll: ScrollState,
    openAdd: () -> Unit,
    openSettings: () -> Unit,
    openMachine: (String) -> Unit,
) {
    val isOn = state == Tunnel.State.On
    val others = model.machines(isOn).filter { !it.isHere && it.id != model.info?.node?.id }
    val hasLinks = model.info?.links?.isNotEmpty() == true
    val problems = model.problems(isOn)
    var failure by remember { mutableStateOf<String?>(null) }
    var isSwitching by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val haptics = LocalHapticFeedback.current

    fun toggle() {
        if (isSwitching) return
        isSwitching = true
        haptics.performHapticFeedback(HapticFeedbackType.ToggleOn)
        scope.launch {
            try {
                if (state == Tunnel.State.Off) model.turnOn() else model.turnOff()
            } catch (e: Exception) {
                failure = e.message
            } finally {
                isSwitching = false
            }
        }
    }

    val here = model.machines(isOn).firstOrNull { it.isHere || it.id == model.info?.node?.id }
    // A network of one has nothing to map.
    val topology = model.topology?.takeIf { others.isNotEmpty() }
    val showsMap = model.showsMap && topology != null
    Page(
        title = stringResource(R.string.machines),
        scroll = scroll,
        actions = {
            if (topology != null) {
                val label = stringResource(if (showsMap) R.string.hide_map else R.string.show_map)
                CircleButton(Icons.Outlined.Hub, label, { model.showMap(!showsMap) }, selected = showsMap)
            }
            CircleButton(Icons.Rounded.Add, stringResource(R.string.add_machine), openAdd)
            CircleButton(Icons.Outlined.Settings, stringResource(R.string.settings), openSettings)
        },
        bottomSpace = ControlBarHeight + 12.dp,
        floating = {
            ControlBar(state, isSwitching, ::toggle, Modifier.align(Alignment.BottomCenter))
        },
    ) {
        Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
            AnimatedVisibility(problems.isNotEmpty(), enter = fadeIn() + expandVertically(), exit = fadeOut() + shrinkVertically()) {
                ProblemList(problems)
            }
            if (others.isEmpty() && !hasLinks) {
                Welcome(openAdd)
            } else {
                AnimatedVisibility(showsMap, enter = fadeIn() + expandVertically(), exit = fadeOut() + shrinkVertically()) {
                    if (topology != null) {
                        NetworkMap(
                            topology, model.machines(isOn), isOn = isOn, open = openMachine,
                            modifier = Modifier
                                .padding(horizontal = 16.dp)
                                .background(MaterialTheme.colorScheme.fill, RoundedCornerShape(20.dp)),
                        )
                    }
                }
                MachineList(listOfNotNull(here) + others, isOn, openMachine)
            }
        }
    }

    failure?.let {
        AlertDialog(
            onDismissRequest = { failure = null },
            confirmButton = { TextButton(onClick = { failure = null }) { Text(stringResource(R.string.ok)) } },
            title = { Text(stringResource(R.string.could_not_turn_on)) },
            text = { Text(it) },
        )
    }
}

/** What is wrong, in gst's words, as `Problems:` in `gst status`. */
@Composable
private fun ProblemList(problems: List<AppModel.Problem>) {
    Column(
        Modifier
            .padding(horizontal = 16.dp)
            .fillMaxWidth()
            .background(statusColors.warningContainer, RoundedCornerShape(20.dp))
            .padding(18.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        for (problem in problems) {
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                Icon(Icons.Rounded.Warning, contentDescription = null, tint = statusColors.warning, modifier = Modifier.size(22.dp))
                Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    val (title, detail) = when (problem) {
                        is AppModel.Problem.Removed ->
                            stringResource(R.string.problem_removed, problem.by, problem.at.ago()) to stringResource(R.string.problem_removed_detail)
                        is AppModel.Problem.Network -> problem.error to problem.hint
                        is AppModel.Problem.Link ->
                            stringResource(R.string.problem_link, problem.destination) to problem.error.takeIf { it.isNotEmpty() }
                    }
                    Text(title.sentence, style = MaterialTheme.typography.titleSmall, color = statusColors.onWarningContainer)
                    if (detail != null) {
                        Text(code(detail.sentence), style = MaterialTheme.typography.bodyMedium, color = statusColors.onWarningContainer.copy(alpha = 0.8f))
                    }
                }
            }
        }
    }
}

/** The machines of the network, this one first, each with whether packets reach it. */
@Composable
private fun MachineList(machines: List<Machine>, isOn: Boolean, open: (String) -> Unit) {
    if (machines.none { !it.isHere }) {
        Footer(stringResource(R.string.machines_empty))
    }
    Group(inset = DotRowInset) {
        for (machine in machines) {
            row {
                ListRow(
                    null,
                    onClick = { open(machine.id) },
                    dot = machine.way.dotColor(),
                    modifier = Modifier.alpha(if (machine.isOnline) 1f else 0.6f),
                    trailing = {
                        // The dot says whether packets reach the machine;
                        // the way they take is on its page. While gst is
                        // off, nothing reaches the machines, which says
                        // nothing of them.
                        val way = machine.way
                        if (machine.isHere) {
                            Tag(stringResource(R.string.this_machine))
                        } else if (isOn && way is Machine.Way.Offline && way.seen != null) {
                            Text(way.seen.ago(), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                        Chevron()
                    },
                ) {
                    Text(machine.name, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    machine.address?.let { Subtitle(it, mono = true) }
                }
            }
        }
    }
}

/** A small gray label, as the one that marks this machine. */
@Composable
private fun Tag(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.labelMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier
            .padding(end = 4.dp)
            .background(MaterialTheme.colorScheme.fill, CircleShape)
            .padding(horizontal = 8.dp, vertical = 3.dp),
    )
}

/** What a machine that has joined no network shows: what gst is, and the one thing to do. */
@Composable
private fun Welcome(add: () -> Unit) {
    Column(Modifier.padding(horizontal = PageMargin), verticalArrangement = Arrangement.spacedBy(24.dp)) {
        Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(stringResource(R.string.welcome_title), fontSize = 22.sp, lineHeight = 28.sp, fontWeight = FontWeight.Bold)
            Text(stringResource(R.string.welcome_text), style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Step(Icons.Outlined.Key, stringResource(R.string.welcome_key))
            Step(Icons.Outlined.Terminal, stringResource(R.string.welcome_authorize))
            Step(Icons.Outlined.Hub, stringResource(R.string.welcome_reach))
        }
        PillButton(stringResource(R.string.add_machine), add, icon = Icons.Rounded.Add)
    }
}

@Composable
private fun Step(icon: ImageVector, text: String) {
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Box(Modifier.size(44.dp).background(MaterialTheme.colorScheme.fill, CircleShape), contentAlignment = Alignment.Center) {
            Icon(icon, contentDescription = null, tint = MaterialTheme.colorScheme.onSurface, modifier = Modifier.size(22.dp))
        }
        Text(code(text), style = MaterialTheme.typography.bodyLarge)
    }
}

/**
 * The bar of glass that floats over the network, a button that turns gst
 * on and off, whose green says that it is on.
 */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun ControlBar(state: Tunnel.State, isSwitching: Boolean, toggle: () -> Unit, modifier: Modifier) {
    val isOn = state == Tunnel.State.On
    val isBusy = isSwitching || state == Tunnel.State.Connecting || state == Tunnel.State.Disconnecting
    val colors = MaterialTheme.colorScheme
    Row(
        modifier
            .navigationBarsPadding()
            .padding(horizontal = 16.dp, vertical = 12.dp)
            .widthIn(max = 640.dp)
            .fillMaxWidth()
            .height(ControlBarHeight)
            .glass(CircleShape)
            .clickable(enabled = !isBusy, role = Role.Button, onClick = toggle),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp, Alignment.CenterHorizontally),
    ) {
        val disc by animateColorAsState(if (isOn) statusColors.good else colors.fill, label = "disc")
        Box(Modifier.size(40.dp).background(disc, CircleShape), contentAlignment = Alignment.Center) {
            if (isBusy) {
                LoadingIndicator(Modifier.size(30.dp), color = if (isOn) Color.White else colors.onSurface)
            } else {
                Icon(
                    Icons.Rounded.PowerSettingsNew,
                    contentDescription = null,
                    tint = if (isOn) Color.White else colors.onSurface,
                    modifier = Modifier.size(22.dp),
                )
            }
        }
        Text(
            when (state) {
                Tunnel.State.Off -> stringResource(R.string.turn_on)
                Tunnel.State.Connecting -> stringResource(R.string.state_turning_on)
                Tunnel.State.On -> stringResource(R.string.state_on)
                Tunnel.State.Disconnecting -> stringResource(R.string.state_turning_off)
            },
            fontSize = 17.sp,
            fontWeight = FontWeight.SemiBold,
            maxLines = 1,
        )
    }
}
