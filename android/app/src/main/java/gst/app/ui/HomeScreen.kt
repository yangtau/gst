package gst.app.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.expandVertically
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.shrinkVertically
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.KeyboardArrowRight
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.Hub
import androidx.compose.material.icons.rounded.Key
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Terminal
import androidx.compose.material.icons.rounded.Tune
import androidx.compose.material.icons.rounded.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialShapes
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SegmentedListItem
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.ToggleButton
import androidx.compose.material3.ToggleButtonDefaults
import androidx.compose.material3.toShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import gst.app.AppModel
import gst.app.Machine
import gst.app.R
import gst.app.Tunnel
import kotlinx.coroutines.launch

/**
 * The network: this machine, the switch that turns gst on, and every
 * machine with the way that packets take to it.
 */
@Composable
fun HomeScreen(
    model: AppModel,
    state: Tunnel.State,
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

    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.page) {
    Box(Modifier.fillMaxSize()) {
        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .statusBarsPadding()
                .padding(horizontal = 16.dp)
                .padding(bottom = 120.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Column(Modifier.widthIn(max = 640.dp), verticalArrangement = Arrangement.spacedBy(24.dp)) {
                Row(Modifier.fillMaxWidth().padding(top = 8.dp), horizontalArrangement = Arrangement.End) {
                    FilledTonalIconButton(onClick = openSettings) {
                        Icon(Icons.Rounded.Tune, contentDescription = stringResource(R.string.settings))
                    }
                }
                Header(model, state, online = others.count { it.isOnline }, total = others.size)
                AnimatedVisibility(problems.isNotEmpty(), enter = fadeIn() + expandVertically(), exit = fadeOut() + shrinkVertically()) {
                    ProblemList(problems)
                }
                if (others.isEmpty() && !hasLinks) {
                    Welcome(openAdd)
                } else {
                    MachineList(others, isOn, openMachine)
                }
            }
        }
        ControlBar(state, isSwitching, ::toggle, openAdd, Modifier.align(Alignment.BottomCenter))
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

/** This machine: its name, its gst IP, and how much of the network it reaches. */
@Composable
private fun Header(model: AppModel, state: Tunnel.State, online: Int, total: Int) {
    val isOn = state == Tunnel.State.On
    val address = model.address(isOn)
    Column(Modifier.padding(horizontal = 8.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            val dot by animateColorAsState(if (isOn) statusColors.good else MaterialTheme.colorScheme.outline, label = "dot")
            StatusDot(dot, filled = isOn, size = 8)
            Text(
                when (state) {
                    Tunnel.State.Off -> stringResource(R.string.state_off)
                    Tunnel.State.Connecting -> stringResource(R.string.state_turning_on)
                    Tunnel.State.Disconnecting -> stringResource(R.string.state_turning_off)
                    Tunnel.State.On -> if (total == 0) stringResource(R.string.state_on) else stringResource(R.string.machines_online, online, total)
                },
                style = MaterialTheme.typography.labelLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Text(
            model.info?.node?.name ?: "gst",
            style = MaterialTheme.typography.displayMediumEmphasized,
            fontWeight = FontWeight.Bold,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
        )
        AnimatedVisibility(address != null, enter = fadeIn() + expandVertically(), exit = fadeOut() + shrinkVertically()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                SelectionContainer {
                    Text(
                        address ?: "",
                        style = MaterialTheme.typography.bodyLarge,
                        fontFamily = FontFamily.Monospace,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                CopyButton(address ?: "")
            }
        }
    }
}

/** What is wrong, in gst's words, as `Problems:` in `gst status`. */
@Composable
private fun ProblemList(problems: List<AppModel.Problem>) {
    Column(
        Modifier
            .fillMaxWidth()
            .background(statusColors.warningContainer, RoundedCornerShape(28.dp))
            .padding(20.dp),
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

/** The other machines of the network. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun MachineList(machines: List<Machine>, isOn: Boolean, open: (String) -> Unit) {
    Column {
        SectionTitle(stringResource(R.string.machines))
        if (machines.isEmpty()) {
            Text(
                stringResource(R.string.machines_empty),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier
                    .fillMaxWidth()
                    .background(MaterialTheme.colorScheme.item, RoundedCornerShape(24.dp))
                    .padding(20.dp),
            )
            return
        }
        Segmented {
            machines.forEachIndexed { i, machine ->
                SegmentedListItem(
                    onClick = { open(machine.id) },
                    shapes = ListItemDefaults.segmentedShapes(i, machines.size),
                    colors = itemColors(),
                    modifier = Modifier.alpha(if (machine.isOnline) 1f else 0.6f),
                    leadingContent = { StatusDot(machine.way) },
                    supportingContent = machine.address?.let { { Text(it, fontFamily = FontFamily.Monospace) } },
                    trailingContent = {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            // While gst is off, no way leads anywhere, which
                            // says nothing of the machines: the row then says
                            // nothing of the way.
                            if (isOn) {
                                Column(horizontalAlignment = Alignment.End, modifier = Modifier.widthIn(max = 150.dp)) {
                                    Text(
                                        machine.way.text(),
                                        style = MaterialTheme.typography.bodyMedium,
                                        textAlign = TextAlign.End,
                                        maxLines = 2,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                    val way = machine.way
                                    if (way is Machine.Way.Offline && way.seen != null) {
                                        Text(way.seen.ago(), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                                    }
                                }
                            }
                            Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, contentDescription = null, tint = MaterialTheme.colorScheme.outline)
                        }
                    },
                ) {
                    Text(machine.name, style = MaterialTheme.typography.titleMediumEmphasized)
                }
            }
        }
    }
}

/** What a machine that has joined no network shows: what gst is, and the one thing to do. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Welcome(add: () -> Unit) {
    Column(
        Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.item, RoundedCornerShape(32.dp))
            .padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(24.dp),
    ) {
        Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(stringResource(R.string.welcome_title), style = MaterialTheme.typography.headlineSmallEmphasized)
            Text(stringResource(R.string.welcome_text), style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Step(Icons.Rounded.Key, MaterialShapes.Cookie6Sided.toShape(), stringResource(R.string.welcome_key))
            Step(Icons.Rounded.Terminal, MaterialShapes.Pill.toShape(), stringResource(R.string.welcome_authorize))
            Step(Icons.Rounded.Hub, MaterialShapes.Sunny.toShape(), stringResource(R.string.welcome_reach))
        }
        Button(
            onClick = add,
            modifier = Modifier.fillMaxWidth().height(ButtonDefaults.MediumContainerHeight),
            contentPadding = ButtonDefaults.contentPaddingFor(ButtonDefaults.MediumContainerHeight),
        ) {
            Icon(Icons.Rounded.Add, contentDescription = null, modifier = Modifier.size(ButtonDefaults.iconSizeFor(ButtonDefaults.MediumContainerHeight)))
            Spacer(Modifier.size(ButtonDefaults.iconSpacingFor(ButtonDefaults.MediumContainerHeight)))
            Text(stringResource(R.string.add_machine), style = ButtonDefaults.textStyleFor(ButtonDefaults.MediumContainerHeight))
        }
    }
}

@Composable
private fun Step(icon: ImageVector, shape: androidx.compose.ui.graphics.Shape, text: String) {
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Box(
            Modifier.size(44.dp).background(MaterialTheme.colorScheme.primaryContainer, shape),
            contentAlignment = Alignment.Center,
        ) {
            Icon(icon, contentDescription = null, tint = MaterialTheme.colorScheme.onPrimaryContainer, modifier = Modifier.size(22.dp))
        }
        Text(code(text), style = MaterialTheme.typography.bodyLarge)
    }
}

/** The controls that float over the network: the switch that turns gst on and off, and the button that adds a machine. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun ControlBar(state: Tunnel.State, isSwitching: Boolean, toggle: () -> Unit, add: () -> Unit, modifier: Modifier) {
    val isOn = state == Tunnel.State.On
    val isBusy = isSwitching || state == Tunnel.State.Connecting || state == Tunnel.State.Disconnecting
    val surface = MaterialTheme.colorScheme.page
    Box(
        modifier
            .fillMaxWidth()
            .background(Brush.verticalGradient(listOf(Color.Transparent, surface.copy(alpha = 0.92f), surface)))
            .navigationBarsPadding()
            .padding(PaddingValues(start = 16.dp, end = 16.dp, top = 24.dp, bottom = 16.dp)),
        contentAlignment = Alignment.Center,
    ) {
        Row(Modifier.widthIn(max = 640.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
            val height = 64.dp
            ToggleButton(
                checked = isOn,
                onCheckedChange = { toggle() },
                enabled = !isBusy,
                shapes = ToggleButtonDefaults.shapesFor(height),
                colors = ToggleButtonDefaults.colors(
                    containerColor = MaterialTheme.colorScheme.surfaceContainerHighest,
                    contentColor = MaterialTheme.colorScheme.onSurface,
                    checkedContainerColor = MaterialTheme.colorScheme.primary,
                    checkedContentColor = MaterialTheme.colorScheme.onPrimary,
                    disabledContainerColor = MaterialTheme.colorScheme.surfaceContainerHighest,
                    disabledContentColor = MaterialTheme.colorScheme.onSurfaceVariant,
                ),
                modifier = Modifier.weight(1f).height(height),
            ) {
                if (isBusy) {
                    LoadingIndicator(Modifier.size(28.dp), color = MaterialTheme.colorScheme.primary)
                } else {
                    Icon(Icons.Rounded.PowerSettingsNew, contentDescription = null, modifier = Modifier.size(24.dp))
                }
                Spacer(Modifier.size(10.dp))
                Text(
                    when (state) {
                        Tunnel.State.Off -> stringResource(R.string.turn_on)
                        Tunnel.State.Connecting -> stringResource(R.string.state_turning_on)
                        Tunnel.State.On -> stringResource(R.string.state_on)
                        Tunnel.State.Disconnecting -> stringResource(R.string.state_turning_off)
                    },
                    style = MaterialTheme.typography.titleMedium,
                )
            }
            FloatingActionButton(
                onClick = add,
                modifier = Modifier.size(height),
                containerColor = MaterialTheme.colorScheme.primaryContainer,
                contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
            ) {
                Icon(Icons.Rounded.Add, contentDescription = stringResource(R.string.add_machine), modifier = Modifier.size(28.dp))
            }
        }
    }
}
