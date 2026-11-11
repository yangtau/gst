package gst.app.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.NetworkPing
import androidx.compose.material.icons.rounded.RemoveCircleOutline
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.SegmentedListItem
import androidx.compose.material3.SheetState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import gst.app.AppModel
import gst.app.Machine
import gst.app.R
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.util.Locale

/** A ping and how it went, in the words of `gst ping`. */
private data class Ping(val text: String, val failed: Boolean)

/** One machine: how to reach it, the way that packets take to it, a ping, and its removal from the network. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MachineSheet(model: AppModel, machineID: String, isOn: Boolean, sheetState: SheetState, close: () -> Unit) {
    ModalBottomSheet(onDismissRequest = close, sheetState = sheetState, containerColor = MaterialTheme.colorScheme.page) {
        val machine = model.machines(isOn).firstOrNull { it.id == machineID }
        if (machine == null) {
            Text(
                stringResource(R.string.machine_gone),
                style = MaterialTheme.typography.titleMedium,
                modifier = Modifier.padding(32.dp).navigationBarsPadding(),
            )
        } else {
            MachineDetails(model, machine, isOn, close)
        }
    }
}

/** A row of a group that says one thing of the machine. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Value(index: Int, count: Int, title: String, value: String, mono: Boolean = false, copy: Boolean = false, leading: (@Composable () -> Unit)? = null) {
    SegmentedListItem(
        shapes = ListItemDefaults.segmentedShapes(index, count),
        colors = itemColors(),
        overlineContent = { Text(title) },
        trailingContent = if (copy) ({ CopyButton(value) }) else null,
    ) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            leading?.invoke()
            SelectionContainer {
                Text(value, fontFamily = if (mono) FontFamily.Monospace else null)
            }
        }
    }
}

@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun MachineDetails(model: AppModel, machine: Machine, isOn: Boolean, close: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val pings = remember { mutableStateListOf<Ping>() }
    var isPinging by remember { mutableStateOf(false) }
    var isConfirmingRemoval by remember { mutableStateOf(false) }
    var failure by remember { mutableStateOf<String?>(null) }
    val direct = stringResource(R.string.way_direct)
    val relay = stringResource(R.string.way_relay)
    val relayVia = stringResource(R.string.way_relay_via)

    Column(
        Modifier
            .verticalScroll(rememberScrollState())
            .padding(horizontal = 16.dp)
            .padding(bottom = 24.dp)
            .navigationBarsPadding(),
        verticalArrangement = Arrangement.spacedBy(20.dp),
    ) {
        Column(Modifier.padding(horizontal = 8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(machine.name, style = MaterialTheme.typography.headlineMediumEmphasized)
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                StatusDot(machine.way)
                Text(machine.way.text(), style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }

        // How to reach it.
        val way = machine.way
        val rows = buildList<@Composable (Int, Int) -> Unit> {
            machine.address?.let { address ->
                add { i, n -> Value(i, n, stringResource(R.string.gst_ip), address, mono = true, copy = true) }
                add { i, n -> Value(i, n, stringResource(R.string.name), machine.hostname, mono = true, copy = true) }
            }
            if (way is Machine.Way.Direct && way.endpoint != null) {
                add { i, n -> Value(i, n, stringResource(R.string.address), way.endpoint, mono = true) }
            }
            if (way is Machine.Way.Offline && way.seen != null) {
                add { i, n -> Value(i, n, stringResource(R.string.last_seen), way.seen.ago()) }
            }
        }
        if (rows.isNotEmpty() || (machine.address == null && machine.isOnline)) {
            Column {
                Segmented { rows.forEachIndexed { i, row -> row(i, rows.size) } }
                if (machine.address == null && machine.isOnline) {
                    Footer(stringResource(R.string.no_gst_ip))
                }
            }
        }

        if (!machine.isHere && machine.isOnline) {
            Column {
                SectionTitle(stringResource(R.string.traffic))
                val traffic = buildList<@Composable (Int, Int) -> Unit> {
                    machine.handshake?.let { add { i, n -> Value(i, n, stringResource(R.string.last_handshake), it.ago()) } }
                    add { i, n -> Value(i, n, stringResource(R.string.received), context.bytes(machine.received)) }
                    add { i, n -> Value(i, n, stringResource(R.string.sent), context.bytes(machine.sent)) }
                }
                Segmented { traffic.forEachIndexed { i, row -> row(i, traffic.size) } }
            }

            // Three pings, a second apart, as `gst ping` sends them.
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                FilledTonalButton(
                    onClick = {
                        scope.launch {
                            isPinging = true
                            pings.clear()
                            repeat(3) { round ->
                                if (round > 0) delay(1000)
                                pings += try {
                                    val reply = model.ping(machine)
                                    var text = if (reply.way == "direct") direct else relay
                                    if (!reply.endpoint.isNullOrEmpty()) {
                                        text += " " + reply.endpoint
                                    } else if (reply.via.isNotEmpty()) {
                                        text = relayVia.format(reply.via.joinToString(", ") { it.name })
                                    }
                                    Ping(text + String.format(Locale.ROOT, ", %.1f ms", reply.timeMs), failed = false)
                                } catch (e: Exception) {
                                    Ping(e.message ?: "", failed = true)
                                }
                            }
                            isPinging = false
                        }
                    },
                    enabled = !isPinging && isOn,
                    modifier = Modifier.fillMaxWidth().height(ButtonDefaults.MediumContainerHeight),
                ) {
                    if (isPinging) {
                        LoadingIndicator(Modifier.size(24.dp))
                    } else {
                        Icon(Icons.Rounded.NetworkPing, contentDescription = null)
                    }
                    Spacer(Modifier.size(8.dp))
                    Text(stringResource(R.string.ping), style = MaterialTheme.typography.titleMedium)
                }
                for (ping in pings) {
                    Text(
                        ping.text,
                        style = MaterialTheme.typography.bodySmall,
                        fontFamily = FontFamily.Monospace,
                        color = if (ping.failed) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 16.dp),
                    )
                }
            }
        }

        val about = buildList<@Composable (Int, Int) -> Unit> {
            machine.software?.let { add { i, n -> Value(i, n, stringResource(R.string.version), it) } }
            add { i, n -> Value(i, n, stringResource(R.string.id), machine.shortID, mono = true) }
        }
        Segmented { about.forEachIndexed { i, row -> row(i, about.size) } }

        if (isOn) {
            Column {
                OutlinedButton(
                    onClick = { isConfirmingRemoval = true },
                    colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
                    modifier = Modifier.fillMaxWidth().height(ButtonDefaults.MediumContainerHeight),
                ) {
                    Icon(Icons.Rounded.RemoveCircleOutline, contentDescription = null)
                    Spacer(Modifier.size(8.dp))
                    Text(
                        stringResource(if (machine.isHere) R.string.leave_network else R.string.remove_from_network),
                        style = MaterialTheme.typography.titleMedium,
                    )
                }
                failure?.let { Footer(it, error = true) }
            }
        }
    }

    if (isConfirmingRemoval) {
        AlertDialog(
            onDismissRequest = { isConfirmingRemoval = false },
            title = {
                Text(if (machine.isHere) stringResource(R.string.leave_question) else stringResource(R.string.remove_question, machine.name))
            },
            text = { Text(stringResource(if (machine.isHere) R.string.leave_text else R.string.remove_text)) },
            confirmButton = {
                TextButton(
                    onClick = {
                        isConfirmingRemoval = false
                        scope.launch {
                            try {
                                model.remove(machine)
                                close()
                            } catch (e: Exception) {
                                failure = e.message
                            }
                        }
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text(stringResource(if (machine.isHere) R.string.leave else R.string.remove)) }
            },
            dismissButton = { TextButton(onClick = { isConfirmingRemoval = false }) { Text(stringResource(R.string.cancel)) } },
        )
    }
}

/** What a group says beneath it. */
@Composable
fun Footer(text: String, error: Boolean = false) {
    Text(
        code(text),
        style = MaterialTheme.typography.bodySmall,
        color = if (error) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
    )
}
