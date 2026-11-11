package gst.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.automirrored.rounded.KeyboardArrowRight
import androidx.compose.material.icons.rounded.Autorenew
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material.icons.rounded.Edit
import androidx.compose.material.icons.rounded.Share
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LargeFlexibleTopAppBar
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedListItem
import androidx.compose.material3.SwipeToDismissBox
import androidx.compose.material3.SwipeToDismissBoxValue
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.rememberSwipeToDismissBoxState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.input.nestedscroll.nestedScroll
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
@OptIn(ExperimentalMaterial3Api::class, ExperimentalMaterial3ExpressiveApi::class)
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
    val scroll = TopAppBarDefaults.exitUntilCollapsedScrollBehavior()
    val nameRule = stringResource(R.string.name_rule)

    BackHandler { close() }

    Scaffold(
        modifier = Modifier.nestedScroll(scroll.nestedScrollConnection),
        topBar = {
            LargeFlexibleTopAppBar(
                title = { Text(stringResource(R.string.settings)) },
                navigationIcon = {
                    IconButton(onClick = close) {
                        Icon(Icons.AutoMirrored.Rounded.ArrowBack, contentDescription = stringResource(R.string.back))
                    }
                },
                scrollBehavior = scroll,
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.page,
                    scrolledContainerColor = MaterialTheme.colorScheme.page,
                ),
            )
        },
        containerColor = MaterialTheme.colorScheme.page,
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState()), contentAlignment = Alignment.TopCenter) {
            Column(
                Modifier.widthIn(max = 640.dp).padding(horizontal = 16.dp).padding(bottom = 24.dp).navigationBarsPadding(),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                // This machine.
                Column {
                    SectionTitle(stringResource(R.string.this_machine))
                    val rows = buildList<@Composable (Int, Int) -> Unit> {
                        add { i, n ->
                            SegmentedListItem(
                                onClick = { isRenaming = true },
                                shapes = ListItemDefaults.segmentedShapes(i, n),
                                colors = itemColors(),
                                overlineContent = { Text(stringResource(R.string.name)) },
                                trailingContent = { Icon(Icons.Rounded.Edit, contentDescription = stringResource(R.string.rename)) },
                            ) { Text(info?.node?.name ?: "") }
                        }
                        info?.node?.id?.let { id -> add { i, n -> Row(i, n, stringResource(R.string.id), id.take(8), mono = true) } }
                        model.topology?.subnet?.takeIf { it.isNotEmpty() }?.let { subnet ->
                            add { i, n -> Row(i, n, stringResource(R.string.subnet), subnet, mono = true) }
                        }
                    }
                    Segmented { rows.forEachIndexed { i, row -> row(i, rows.size) } }
                    Footer(nameFailure ?: stringResource(R.string.reach_as, info?.node?.name ?: ""), error = nameFailure != null)
                }

                // The key that servers authorize.
                info?.publicKey?.let { key ->
                    Column {
                        SectionTitle(stringResource(R.string.ssh_key))
                        Column(
                            Modifier
                                .fillMaxWidth()
                                .clip(MaterialTheme.shapes.large)
                                .background(MaterialTheme.colorScheme.item)
                                .padding(16.dp),
                            verticalArrangement = Arrangement.spacedBy(16.dp),
                        ) {
                            SelectionContainer {
                                Text(
                                    key,
                                    style = MaterialTheme.typography.bodySmall,
                                    fontFamily = FontFamily.Monospace,
                                    maxLines = 3,
                                    overflow = TextOverflow.MiddleEllipsis,
                                )
                            }
                            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                CopyKeyButton(key)
                                OutlinedButton(onClick = { context.share(key) }) {
                                    Icon(Icons.Rounded.Share, contentDescription = null, modifier = Modifier.size(18.dp))
                                    Spacer(Modifier.size(8.dp))
                                    Text(stringResource(R.string.share))
                                }
                                TextButton(
                                    onClick = { isConfirmingNewKey = true },
                                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                                ) {
                                    Icon(Icons.Rounded.Autorenew, contentDescription = null, modifier = Modifier.size(18.dp))
                                    Spacer(Modifier.size(8.dp))
                                    Text(stringResource(R.string.new_key))
                                }
                            }
                        }
                        Footer(keyFailure ?: stringResource(R.string.key_footer), error = keyFailure != null)
                    }
                }

                // The links.
                Column {
                    SectionTitle(stringResource(R.string.links))
                    val links = info?.links ?: emptyList()
                    Segmented {
                        links.forEachIndexed { i, link ->
                            LinkRow(
                                i, links.size, link.destination,
                                model.status?.links?.firstOrNull { it.destination == link.destination },
                            ) {
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
                    Footer(
                        linkFailure ?: stringResource(if (links.isEmpty()) R.string.links_none else R.string.links_footer),
                        error = linkFailure != null,
                    )
                }

                // What helps when something does not work.
                Segmented {
                    val count = if (info?.node?.software != null) 2 else 1
                    SegmentedListItem(
                        onClick = openLog,
                        shapes = ListItemDefaults.segmentedShapes(0, count),
                        colors = itemColors(),
                        trailingContent = { Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, contentDescription = null) },
                    ) { Text(stringResource(R.string.log)) }
                    info?.node?.software?.let { Row(1, count, stringResource(R.string.version), it) }
                }
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

@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Row(index: Int, count: Int, title: String, value: String, mono: Boolean = false) {
    SegmentedListItem(
        shapes = ListItemDefaults.segmentedShapes(index, count),
        colors = itemColors(),
        overlineContent = { Text(title) },
    ) {
        SelectionContainer { Text(value, fontFamily = if (mono) FontFamily.Monospace else null) }
    }
}

@Composable
private fun CopyKeyButton(key: String) {
    val clipboard = androidx.compose.ui.platform.LocalClipboard.current
    val scope = rememberCoroutineScope()
    var isCopied by remember { mutableStateOf(false) }
    FilledTonalButton(onClick = {
        scope.launch {
            clipboard.setClipEntry(androidx.compose.ui.platform.ClipEntry(android.content.ClipData.newPlainText("gst", key)))
            isCopied = true
            kotlinx.coroutines.delay(1500)
            isCopied = false
        }
    }) {
        Icon(
            if (isCopied) Icons.Rounded.Check else Icons.Rounded.ContentCopy,
            contentDescription = null,
            modifier = Modifier.size(18.dp),
        )
        Spacer(Modifier.size(8.dp))
        Text(stringResource(R.string.copy_key))
    }
}

/** A link, named by its destination, and how it goes; swiped away, it is removed. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun LinkRow(index: Int, count: Int, destination: String, state: DaemonStatus.Link?, unlink: () -> Unit) {
    val swipe = rememberSwipeToDismissBoxState()
    val shapes = ListItemDefaults.segmentedShapes(index, count)
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
        onDismiss = { if (it == SwipeToDismissBoxValue.EndToStart) unlink() },
        backgroundContent = {
            Box(
                Modifier.fillMaxSize().clip(shapes.shape).background(MaterialTheme.colorScheme.errorContainer).padding(horizontal = 24.dp),
                contentAlignment = Alignment.CenterEnd,
            ) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(stringResource(R.string.unlink), color = MaterialTheme.colorScheme.onErrorContainer, style = MaterialTheme.typography.labelLarge)
                    Icon(Icons.Rounded.Delete, contentDescription = null, tint = MaterialTheme.colorScheme.onErrorContainer)
                }
            }
        },
    ) {
        SegmentedListItem(
            shapes = shapes,
            colors = itemColors(),
            supportingContent = {
                Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        StatusDot(color, size = 8)
                        Text(summary)
                    }
                    val error = state?.error
                    if (state?.state == DaemonStatus.Link.State.Down && !error.isNullOrEmpty()) {
                        Text(code(error.sentence), color = MaterialTheme.colorScheme.error)
                    }
                }
            },
        ) {
            Text(destination, fontFamily = FontFamily.Monospace, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
        }
    }
}
