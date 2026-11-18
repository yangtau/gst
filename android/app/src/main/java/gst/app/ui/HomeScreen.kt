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
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.Icon
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.DpOffset
import androidx.compose.ui.unit.dp
import gst.app.AppModel
import gst.app.Machine
import gst.app.Pings
import gst.app.R
import gst.app.Tunnel
import kotlinx.coroutines.launch

/** The height of the bar of glass that floats over the bottom of the network. */
private val BottomBarHeight = 56.dp

/** What comes up over the network, from the bottom of the screen. */
sealed interface HomeSheet {
    /** The map of the links. */
    data object Map : HomeSheet

    /** The pings of a machine. */
    data class Ping(val machineID: String) : HomeSheet
}

/**
 * The network: its machines, this one first, each with whether packets
 * reach it, and floating over their end a bar: the switch that turns gst
 * on and off at its start, and at its end the buttons that open the map of
 * the links, the adding of a machine, and the settings. While gst is off,
 * the screen offers to turn it on in place of the machines, which it does
 * not reach then. [sheet] is what shows over the network, which [show]
 * changes, and [scroll] is kept while a screen opens over it; a demo gives
 * the pings that the sheet of a machine's pings starts with.
 */
@Composable
fun HomeScreen(
    model: AppModel,
    state: Tunnel.State,
    scroll: ScrollState,
    sheet: HomeSheet?,
    show: (HomeSheet?) -> Unit,
    openAdd: () -> Unit,
    openSettings: () -> Unit,
    openMachine: (String) -> Unit,
    pings: Pings = Pings(),
) {
    val isOn = state == Tunnel.State.On
    // This machine, by its id: while gst is off, nothing says it is here.
    val machines = model.machines(isOn)
    val me = model.info?.node?.id
    val others = machines.filter { it.id != me }
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

    val isBusy = isSwitching || state == Tunnel.State.Connecting || state == Tunnel.State.Disconnecting
    val isWelcome = others.isEmpty() && !hasLinks
    // Off, as far as the app knows: until gst is open, it may be about to
    // turn on again by itself, as after the phone restarted.
    val isOff = !isWelcome && state == Tunnel.State.Off && model.info != null
    // A network of one has nothing to map, and one that gst does not reach now is not shown.
    val topology = model.topology?.takeIf { others.isNotEmpty() && !isOff }
    Page(
        title = null,
        scroll = scroll,
        bottomSpace = BottomBarHeight + 12.dp,
        floating = {
            if (isOff) {
                Off(isBusy, ::toggle, Modifier.align(Alignment.Center))
            }
            BottomBar(
                isOn, isBusy, ::toggle,
                openMap = if (topology != null) ({ show(HomeSheet.Map) }) else null,
                openAdd = openAdd,
                openSettings = openSettings,
                modifier = Modifier.align(Alignment.BottomCenter),
            )
        },
    ) {
        if (!isOff) {
            Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
                AnimatedVisibility(problems.isNotEmpty(), enter = fadeIn() + expandVertically(), exit = fadeOut() + shrinkVertically()) {
                    ProblemList(problems)
                }
                if (isWelcome) {
                    Welcome(openAdd)
                } else {
                    MachineList(
                        machines, me, isOn,
                        canPing = { model.canPing(it, isOn) },
                        open = openMachine,
                        ping = { show(HomeSheet.Ping(it)) },
                    )
                }
            }
        }
    }

    when (sheet) {
        HomeSheet.Map -> if (topology != null) {
            Sheet(stringResource(R.string.map), close = { show(null) }) {
                NetworkMap(
                    topology, machines, isOn = isOn,
                    open = {
                        show(null)
                        openMachine(it)
                    },
                )
            }
        } else {
            // Nothing to map any more, as once gst is off: the map does not come back by itself.
            LaunchedEffect(Unit) { show(null) }
        }
        is HomeSheet.Ping -> PingSheet(model, sheet.machineID, isOn, pings, close = { show(null) })
        null -> {}
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

/**
 * The machines of the network, this one first, each with whether packets
 * reach it. A tap opens a machine, and a touch that holds one that a ping
 * may reach offers to ping it.
 */
@Composable
private fun MachineList(
    machines: List<Machine>,
    me: String?,
    isOn: Boolean,
    canPing: (Machine) -> Boolean,
    open: (String) -> Unit,
    ping: (String) -> Unit,
) {
    if (machines.all { it.id == me }) {
        Footer(stringResource(R.string.machines_empty))
    }
    // The machine whose menu shows.
    var held by remember { mutableStateOf<String?>(null) }
    Group(inset = DotRowInset) {
        for (machine in machines) {
            row {
                Box {
                    DropdownMenu(
                        expanded = held == machine.id,
                        onDismissRequest = { held = null },
                        offset = DpOffset(DotRowInset - 12.dp, 0.dp),
                        shape = RoundedCornerShape(16.dp),
                        containerColor = MaterialTheme.colorScheme.surfaceContainer,
                    ) {
                        DropdownMenuItem(
                            text = { Text(stringResource(R.string.ping), style = MaterialTheme.typography.bodyLarge) },
                            leadingIcon = { Icon(PingIcon, contentDescription = null) },
                            onClick = {
                                held = null
                                ping(machine.id)
                            },
                        )
                    }
                    ListRow(
                        null,
                        onClick = { open(machine.id) },
                        onLongClick = if (canPing(machine)) ({ held = machine.id }) else null,
                        dot = machine.way.dotColor(),
                        modifier = Modifier.alpha(if (machine.isOnline) 1f else 0.6f),
                        trailing = {
                            // The dot says whether packets reach the machine;
                            // the way they take is on its page. While gst is
                            // off, nothing reaches the machines, which says
                            // nothing of them.
                            val way = machine.way
                            if (machine.id == me) {
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
            Text(stringResource(R.string.welcome_title), style = TitleStyle)
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
 * What the network shows while gst is off: that it is, and the button that
 * turns it on, in the middle of the screen, where it is the one thing to
 * do.
 */
@Composable
private fun Off(isBusy: Boolean, turnOn: () -> Unit, modifier: Modifier) {
    Notice(Icons.Rounded.PowerSettingsNew, stringResource(R.string.off_title), stringResource(R.string.off_text), modifier) {
        PillButton(stringResource(R.string.turn_on), turnOn, modifier = Modifier.widthIn(min = 180.dp), enabled = !isBusy)
    }
}

/**
 * What floats over the end of the network, in glass: the switch, and the
 * buttons that open something, which [openMap] is one of while there is a
 * map to show.
 */
@Composable
private fun BottomBar(
    isOn: Boolean,
    isBusy: Boolean,
    toggle: () -> Unit,
    openMap: (() -> Unit)?,
    openAdd: () -> Unit,
    openSettings: () -> Unit,
    modifier: Modifier,
) {
    Row(
        modifier
            .navigationBarsPadding()
            .padding(horizontal = PageMargin, vertical = 12.dp)
            .widthIn(max = 640.dp)
            .fillMaxWidth(),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Switch(isOn, isBusy, toggle)
        Row(Modifier.height(BottomBarHeight).floating().padding(horizontal = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            if (openMap != null) {
                BarButton(Icons.Outlined.Hub, stringResource(R.string.show_map), openMap)
            }
            BarButton(Icons.Rounded.Add, stringResource(R.string.add_machine), openAdd)
            BarButton(Icons.Outlined.Settings, stringResource(R.string.settings), openSettings)
        }
    }
}

/** Round glass a little above the page, by its shadow. */
@Composable
private fun Modifier.floating(): Modifier =
    shadow(12.dp, CircleShape, clip = false, ambientColor = Color.Black.copy(alpha = 0.3f), spotColor = Color.Black.copy(alpha = 0.3f)).glass(CircleShape)

/**
 * The switch that turns gst on and off: a round button of glass, whose
 * icon alone is green while gst is on.
 */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Switch(isOn: Boolean, isBusy: Boolean, toggle: () -> Unit) {
    val label = stringResource(if (isOn) R.string.turn_off else R.string.turn_on)
    val tint by animateColorAsState(if (isOn) statusColors.good else MaterialTheme.colorScheme.onSurface, label = "switch")
    Box(
        Modifier
            .size(BottomBarHeight)
            .floating()
            .clickable(enabled = !isBusy, role = Role.Button, onClick = toggle)
            .semantics { contentDescription = label },
        contentAlignment = Alignment.Center,
    ) {
        if (isBusy) {
            LoadingIndicator(Modifier.size(32.dp), color = tint)
        } else {
            Icon(Icons.Rounded.PowerSettingsNew, contentDescription = null, tint = tint, modifier = Modifier.size(26.dp))
        }
    }
}

/** A button of the bar at the bottom: an icon, on the bar's glass. */
@Composable
private fun BarButton(icon: ImageVector, label: String, onClick: () -> Unit) {
    Box(
        Modifier
            .size(48.dp)
            .clip(CircleShape)
            .clickable(role = Role.Button, onClick = onClick)
            .semantics { contentDescription = label },
        contentAlignment = Alignment.Center,
    ) {
        Icon(icon, contentDescription = null, tint = MaterialTheme.colorScheme.onSurface, modifier = Modifier.size(24.dp))
    }
}
