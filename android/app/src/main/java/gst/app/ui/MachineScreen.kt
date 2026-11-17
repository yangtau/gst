package gst.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import gst.app.AppModel
import gst.app.DaemonStatus
import gst.app.Machine
import gst.app.Pings
import gst.app.R
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds
import kotlin.time.TimeSource
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * One machine, on a page of its own: the pings that the app sends it while
 * the page shows it, how to reach it, the ports it exposes, and its
 * removal from the network. [initial] are the pings that the page starts
 * with, which a demo gives it.
 */
@Composable
fun MachineScreen(model: AppModel, machineID: String, isOn: Boolean, initial: Pings = Pings(), close: () -> Unit) {
    BackHandler { close() }
    val back = @Composable { CircleButton(Icons.AutoMirrored.Rounded.ArrowBack, stringResource(R.string.back), close) }
    val machine = model.machines(isOn).firstOrNull { it.id == machineID }
    if (machine == null) {
        Page("", navigation = back) {
            Text(stringResource(R.string.machine_gone), style = MaterialTheme.typography.titleMedium, modifier = Modifier.padding(horizontal = PageMargin))
        }
        return
    }
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var pings by remember { mutableStateOf(initial) }
    var remoteExposes by remember { mutableStateOf<List<DaemonStatus.Expose>>(emptyList()) }
    var isConfirmingRemoval by remember { mutableStateOf(false) }
    var failure by remember { mutableStateOf<String?>(null) }

    // The ports the machine exposes: this machine's from its status, which
    // the app keeps up to date, another's from its daemon, now and then
    // while the page shows it.
    val exposes = if (machine.isHere) model.status?.exposes.orEmpty() else remoteExposes
    if (isOn && machine.isOnline && !machine.isHere) {
        RepeatWhileShown(machine.id, 30.seconds) {
            try {
                remoteExposes = model.exposes(machine)
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
            }
        }
    }

    // The app pings the machine once a second, for as long as the page
    // shows it, the app is on the screen, and packets may reach it. The
    // VPN keeps the process when the app leaves the screen, and the pings
    // stop then.
    val isPinged = isOn && !machine.isHere && machine.isOnline
    if (isPinged) {
        RepeatWhileShown(machine.id, 1.seconds) {
            pings = try {
                val reply = model.ping(machine)
                pings.answered(reply.timeMs, if (reply.way == "direct") Machine.Way.Direct(reply.endpoint) else Machine.Way.Relay(reply.via.map { it.name }))
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                pings.failed(e.message ?: "")
            }
        }
    }

    Page(
        title = machine.name,
        navigation = back,
        subtitle = if (isPinged) null else {
            {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    StatusDot(machine.way)
                    Text(machine.way.text(), style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        },
    ) {
      Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
        if (isPinged) {
            PingView(pings, machine.way, Modifier.padding(horizontal = 16.dp))
        }

        // How to reach it.
        val way = machine.way
        Group(footer = if (machine.address == null && machine.isOnline) stringResource(R.string.no_gst_ip) else null) {
            machine.address?.let { address ->
                row { ValueRow(stringResource(R.string.gst_ip), address, mono = true, copy = true) }
                row { ValueRow(stringResource(R.string.name), machine.hostname, mono = true, copy = true) }
            }
            if (way is Machine.Way.Offline && way.seen != null) {
                row { ValueRow(stringResource(R.string.last_seen), way.seen.ago()) }
            }
        }

        if (exposes.isNotEmpty()) {
            Group(header = stringResource(R.string.exposed_ports)) {
                for (expose in exposes) {
                    row { ExposeRow(machine, expose) }
                }
            }
        }

        if (!machine.isHere && machine.isOnline) {
            Group(header = stringResource(R.string.traffic)) {
                machine.handshake?.let { row { ValueRow(stringResource(R.string.last_handshake), it.ago()) } }
                row { ValueRow(stringResource(R.string.received), context.bytes(machine.received)) }
                row { ValueRow(stringResource(R.string.sent), context.bytes(machine.sent)) }
            }
        }

        Group {
            machine.software?.let { row { ValueRow(stringResource(R.string.version), it) } }
            row { ValueRow(stringResource(R.string.id), machine.shortID, mono = true) }
            if (isOn) {
                row {
                    ListRow(null, onClick = { isConfirmingRemoval = true }, tint = MaterialTheme.colorScheme.error) {
                        Text(stringResource(if (machine.isHere) R.string.leave_network else R.string.remove_from_network))
                    }
                }
            }
        }
        failure?.let { Footer(it, isError = true) }
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

/** A port that the machine exposes: where other machines reach it, which a button copies, and how it goes. */
@Composable
private fun ExposeRow(machine: Machine, expose: DaemonStatus.Expose) {
    val address = "${machine.hostname}:${expose.port}"
    ListRow(null, trailing = { CopyButton(address, modifier = Modifier.size(36.dp)) }) {
        Text(address, fontFamily = FontFamily.Monospace, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
        val protocol = expose.protocol.uppercase()
        if (expose.state == "running") {
            Subtitle(protocol)
        } else {
            Subtitle("$protocol · ${expose.error?.sentence ?: expose.state}", color = statusColors.warning)
        }
    }
}

/**
 * Does [work] every [period], counted from when each round starts, while
 * the app is on the screen and the page shows [key].
 */
@Composable
private fun RepeatWhileShown(key: Any, period: Duration, work: suspend () -> Unit) {
    val current by rememberUpdatedState(work)
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    LaunchedEffect(key, lifecycle) {
        lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
            while (true) {
                val started = TimeSource.Monotonic.markNow()
                current()
                delay(period - started.elapsedNow())
            }
        }
    }
}
