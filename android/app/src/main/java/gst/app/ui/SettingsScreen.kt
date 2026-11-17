package gst.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.outlined.Autorenew
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SwipeToDismissBox
import androidx.compose.material3.SwipeToDismissBoxValue
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberSwipeToDismissBoxState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import gst.app.AppModel
import gst.app.DaemonStatus
import gst.app.R
import kotlinx.coroutines.launch

/** This machine: its name, the key that servers authorize, its links, and what helps when something does not work. */
@Composable
fun SettingsScreen(model: AppModel, close: () -> Unit, openLog: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var nameFailure by remember { mutableStateOf<String?>(null) }
    var linkFailure by remember { mutableStateOf<String?>(null) }
    var keyFailure by remember { mutableStateOf<String?>(null) }
    var isRenaming by remember { mutableStateOf(false) }
    var isConfirmingNewKey by remember { mutableStateOf(false) }
    val info = model.info
    val nameRule = stringResource(R.string.name_rule)

    BackHandler { close() }

    Page(
        title = stringResource(R.string.settings),
        navigation = { CircleButton(Icons.AutoMirrored.Rounded.ArrowBack, stringResource(R.string.back), close) },
    ) {
        Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
            // This machine.
            Group(
                header = stringResource(R.string.this_machine),
                footer = nameFailure ?: stringResource(R.string.reach_as, info?.node?.name ?: ""),
                isError = nameFailure != null,
            ) {
                row { ValueRow(stringResource(R.string.name), info?.node?.name ?: "", onClick = { isRenaming = true }) }
                info?.node?.id?.let { id -> row { ValueRow(stringResource(R.string.id), id.take(8), mono = true) } }
                model.topology?.subnet?.takeIf { it.isNotEmpty() }?.let { subnet ->
                    row { ValueRow(stringResource(R.string.subnet), subnet, mono = true) }
                }
            }

            // The key that servers authorize.
            info?.publicKey?.let { key ->
                Column {
                    SectionHeader(stringResource(R.string.ssh_key))
                    CodeBlock(key, Modifier.padding(horizontal = 16.dp).padding(top = 4.dp, bottom = 4.dp), maxLines = 3)
                    Group(inset = RowInset, footer = keyFailure ?: stringResource(R.string.key_footer), isError = keyFailure != null) {
                        row { CopyKeyRow(key) }
                        row {
                            ListRow(Icons.Outlined.Share, onClick = { context.share(key) }) { Text(stringResource(R.string.share)) }
                        }
                        row {
                            ListRow(Icons.Outlined.Autorenew, onClick = { isConfirmingNewKey = true }, tint = MaterialTheme.colorScheme.error) {
                                Text(stringResource(R.string.new_key))
                            }
                        }
                    }
                }
            }

            // The links.
            val links = info?.links ?: emptyList()
            Group(
                inset = DotRowInset,
                header = stringResource(R.string.links),
                footer = linkFailure ?: stringResource(if (links.isEmpty()) R.string.links_none else R.string.links_footer),
                isError = linkFailure != null,
            ) {
                for (link in links) {
                    // Its swipe and its question go with the link, not with its place in the list.
                    row {
                        key(link.destination) {
                            LinkRow(link.destination, model.status?.links?.firstOrNull { it.destination == link.destination }) {
                                scope.launch {
                                    try {
                                        model.unlink(link.destination)
                                        linkFailure = null
                                    } catch (e: Exception) {
                                        linkFailure = e.message
                                    }
                                }
                            }
                        }
                    }
                }
            }

            // What helps when something does not work.
            Group {
                row {
                    ListRow(null, onClick = openLog, trailing = { Chevron() }) { Text(stringResource(R.string.log)) }
                }
                info?.node?.software?.let { row { ValueRow(stringResource(R.string.version), it) } }
            }
        }
    }

    if (isRenaming) {
        var name by remember { mutableStateOf(info?.node?.name ?: "") }
        // `gst name`, with its rule for names.
        fun rename() {
            val wanted = name.trim().lowercase()
            isRenaming = false
            if (wanted == info?.node?.name) {
                nameFailure = null
                return
            }
            if (!Regex("^[a-z0-9][a-z0-9-]{0,31}$").matches(wanted)) {
                nameFailure = nameRule
                return
            }
            scope.launch {
                try {
                    model.rename(wanted)
                    nameFailure = null
                } catch (e: Exception) {
                    nameFailure = e.message
                }
            }
        }
        AlertDialog(
            onDismissRequest = { isRenaming = false },
            title = { Text(stringResource(R.string.name)) },
            text = {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    singleLine = true,
                    supportingText = { Text(stringResource(R.string.name_rule)) },
                    keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.None, autoCorrectEnabled = false, imeAction = ImeAction.Done),
                    keyboardActions = KeyboardActions(onDone = { rename() }),
                )
            },
            confirmButton = { TextButton(onClick = ::rename) { Text(stringResource(R.string.done)) } },
            dismissButton = { TextButton(onClick = { isRenaming = false }) { Text(stringResource(R.string.cancel)) } },
        )
    }

    if (isConfirmingNewKey) {
        AlertDialog(
            onDismissRequest = { isConfirmingNewKey = false },
            title = { Text(stringResource(R.string.new_key_question)) },
            text = { Text(code(stringResource(R.string.new_key_text))) },
            confirmButton = {
                TextButton(
                    onClick = {
                        isConfirmingNewKey = false
                        scope.launch {
                            try {
                                model.newKey()
                                keyFailure = null
                            } catch (e: Exception) {
                                keyFailure = e.message
                            }
                        }
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text(stringResource(R.string.new_key)) }
            },
            dismissButton = { TextButton(onClick = { isConfirmingNewKey = false }) { Text(stringResource(R.string.cancel)) } },
        )
    }
}

/** The row that copies the key, and says for a moment that it did. */
@Composable
private fun CopyKeyRow(key: String) {
    val (isCopied, copy) = rememberCopy(key)
    ListRow(copyIcon(isCopied), onClick = copy) { Text(stringResource(R.string.copy_key)) }
}

/** A link, named by its destination, and how it goes; swiped away, it is removed once the person says so. */
@Composable
private fun LinkRow(destination: String, state: DaemonStatus.Link?, unlink: () -> Unit) {
    val swipe = rememberSwipeToDismissBoxState()
    val scope = rememberCoroutineScope()
    var isConfirming by remember { mutableStateOf(false) }
    if (isConfirming) {
        fun cancel() {
            isConfirming = false
            scope.launch { swipe.reset() }
        }
        AlertDialog(
            onDismissRequest = ::cancel,
            title = { Text(stringResource(R.string.unlink_question, destination)) },
            text = { Text(stringResource(R.string.unlink_text)) },
            confirmButton = {
                TextButton(
                    onClick = {
                        isConfirming = false
                        unlink()
                        scope.launch { swipe.reset() }
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text(stringResource(R.string.unlink)) }
            },
            dismissButton = { TextButton(onClick = ::cancel) { Text(stringResource(R.string.cancel)) } },
        )
    }
    val colors = statusColors
    val (color, summary) = when (state?.state) {
        DaemonStatus.Link.State.Running ->
            colors.good to (state.peer?.name?.let { stringResource(R.string.link_running_to, it) } ?: stringResource(R.string.link_running))
        DaemonStatus.Link.State.Connecting -> colors.warning to stringResource(R.string.link_connecting)
        DaemonStatus.Link.State.Down -> MaterialTheme.colorScheme.error to stringResource(R.string.link_down)
        null -> MaterialTheme.colorScheme.outline to stringResource(R.string.state_off)
    }
    SwipeToDismissBox(
        state = swipe,
        enableDismissFromStartToEnd = false,
        onDismiss = { if (it == SwipeToDismissBoxValue.EndToStart) isConfirming = true },
        backgroundContent = {
            Box(
                Modifier.fillMaxSize().background(MaterialTheme.colorScheme.error).padding(horizontal = PageMargin),
                contentAlignment = Alignment.CenterEnd,
            ) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(stringResource(R.string.unlink), color = MaterialTheme.colorScheme.onError, style = MaterialTheme.typography.labelLarge)
                    Icon(Icons.Rounded.Delete, contentDescription = null, tint = MaterialTheme.colorScheme.onError)
                }
            }
        },
    ) {
        ListRow(null, dot = color, modifier = Modifier.background(MaterialTheme.colorScheme.background)) {
            Text(destination, fontFamily = FontFamily.Monospace, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
            Subtitle(summary)
            val error = state?.error
            if (state?.state == DaemonStatus.Link.State.Down && !error.isNullOrEmpty()) {
                Text(code(error.sentence), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
            }
        }
    }
}
