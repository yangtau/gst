package gst.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.Schedule
import androidx.compose.material.icons.rounded.Share
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ContainedLoadingIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialShapes
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.toShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import gst.app.AppModel
import gst.app.Destination
import gst.app.R
import gst.app.ServerCheck
import kotlinx.coroutines.launch

/** A step of adding a machine. */
sealed interface AddStep {
    /** Asking where the machine is. */
    data object Form : AddStep

    /** Logging in to see how it goes. */
    data object Checking : AddStep

    /** The server showed a host key that is not recorded for it. */
    data class HostKey(val check: ServerCheck) : AddStep

    /** The server does not accept this machine's key. */
    data class Authorize(val check: ServerCheck) : AddStep

    /** The link is added, and gst is connecting it. */
    data object Joining : AddStep

    /** The link runs; the machine at its other end is called this. */
    data class Joined(val name: String) : AddStep

    /** The link is added but does not run; gst says why, if it knows. */
    data class Waiting(val reason: String?) : AddStep

    data class Failed(val message: String) : AddStep
}

/**
 * Adds a machine, as `gst add` does, with the steps that ssh takes care of
 * elsewhere: checking the server's host key, and authorizing this
 * machine's key on it.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AddMachineScreen(model: AppModel, close: () -> Unit, initialStep: AddStep = AddStep.Form) {
    var server by rememberSaveable { mutableStateOf("") }
    var port by rememberSaveable { mutableStateOf("") }
    var step by remember { mutableStateOf(initialStep) }
    val scope = rememberCoroutineScope()
    val destination = Destination.make(server, port)
    val isBusy = step == AddStep.Checking || step == AddStep.Joining

    BackHandler(enabled = !isBusy) { close() }

    fun join(destination: String) {
        scope.launch {
            step = AddStep.Joining
            try {
                model.add(destination)
            } catch (e: Exception) {
                step = AddStep.Failed(e.message ?: "")
                return@launch
            }
            step = when (val outcome = model.watchLink(destination)) {
                is AppModel.LinkOutcome.Running -> AddStep.Joined(outcome.name)
                is AppModel.LinkOutcome.Failed -> AddStep.Waiting(outcome.error)
                AppModel.LinkOutcome.Pending -> AddStep.Waiting(null)
            }
        }
    }

    fun check() {
        val destination = destination ?: return
        step = AddStep.Checking
        scope.launch {
            step = try {
                val check = model.check(destination)
                when (check.state) {
                    ServerCheck.State.Ready -> {
                        join(destination)
                        return@launch
                    }
                    ServerCheck.State.UnknownHost, ServerCheck.State.ChangedHost -> AddStep.HostKey(check)
                    ServerCheck.State.Unauthorized -> AddStep.Authorize(check)
                    ServerCheck.State.Failed -> AddStep.Failed(check.error ?: "")
                }
            } catch (e: Exception) {
                AddStep.Failed(e.message ?: "")
            }
        }
    }

    fun trust(key: ServerCheck.HostKey) {
        val destination = destination ?: return
        scope.launch {
            try {
                model.trust(key, destination)
                check()
            } catch (e: Exception) {
                step = AddStep.Failed(e.message ?: "")
            }
        }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(stringResource(R.string.add_machine)) },
                navigationIcon = {
                    IconButton(onClick = close, enabled = !isBusy) {
                        Icon(Icons.Rounded.Close, contentDescription = stringResource(R.string.close))
                    }
                },
            )
        },
        containerColor = MaterialTheme.colorScheme.surface,
    ) { padding ->
        Box(
            Modifier
                .fillMaxSize()
                .padding(padding)
                .imePadding()
                .verticalScroll(rememberScrollState()),
            contentAlignment = Alignment.TopCenter,
        ) {
            val enter = MaterialTheme.motionScheme.defaultEffectsSpec<Float>()
            val exit = MaterialTheme.motionScheme.fastEffectsSpec<Float>()
            AnimatedContent(
                step,
                transitionSpec = { fadeIn(enter) togetherWith fadeOut(exit) },
                contentKey = { it::class },
                label = "step",
                modifier = Modifier.widthIn(max = 560.dp).padding(horizontal = 24.dp).padding(bottom = 24.dp),
            ) { current ->
                Column(verticalArrangement = Arrangement.spacedBy(24.dp)) {
                    when (current) {
                        AddStep.Form -> Form(server, { server = it }, port, { port = it }, destination != null, ::check)
                        AddStep.Checking -> Progress(stringResource(R.string.connecting_server))
                        is AddStep.HostKey -> HostKey(current.check, ::trust)
                        is AddStep.Authorize -> Authorize(current.check, model.info?.publicKey ?: "", ::check)
                        AddStep.Joining -> Progress(stringResource(R.string.joining))
                        is AddStep.Joined -> Outcome(
                            Icons.Rounded.Check, statusColors.goodContainer, statusColors.onGoodContainer,
                            stringResource(R.string.joined_title, current.name), stringResource(R.string.joined_text), close,
                        )
                        is AddStep.Waiting -> Waiting(current.reason, close)
                        is AddStep.Failed -> Failed(current.message, ::check) { step = AddStep.Form }
                    }
                }
            }
        }
    }
}

@Composable
private fun Heading(title: String, text: String) {
    Column(Modifier.padding(top = 8.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text(title, style = MaterialTheme.typography.headlineMediumEmphasized)
        Text(code(text), style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun Caption(text: String) {
    Text(text, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
}

@Composable
private fun PrimaryButton(text: String, enabled: Boolean = true, colors: androidx.compose.material3.ButtonColors = ButtonDefaults.buttonColors(), action: () -> Unit) {
    val height = ButtonDefaults.MediumContainerHeight
    Button(
        onClick = action,
        enabled = enabled,
        colors = colors,
        shapes = ButtonDefaults.shapesFor(height),
        contentPadding = ButtonDefaults.contentPaddingFor(height),
        modifier = Modifier.fillMaxWidth().height(height),
    ) {
        Text(text, style = ButtonDefaults.textStyleFor(height))
    }
}

@Composable
private fun Form(server: String, setServer: (String) -> Unit, port: String, setPort: (String) -> Unit, isValid: Boolean, next: () -> Unit) {
    val focus = remember { FocusRequester() }
    Heading(stringResource(R.string.where_title), stringResource(R.string.where_text))
    Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        val mono = TextStyle(fontFamily = FontFamily.Monospace)
        OutlinedTextField(
            value = server,
            onValueChange = setServer,
            label = { Text(stringResource(R.string.login)) },
            placeholder = { Text("user@host", style = mono) },
            singleLine = true,
            textStyle = MaterialTheme.typography.bodyLarge.merge(mono),
            keyboardOptions = KeyboardOptions(
                capitalization = KeyboardCapitalization.None, autoCorrectEnabled = false,
                keyboardType = KeyboardType.Email, imeAction = ImeAction.Go,
            ),
            keyboardActions = KeyboardActions(onGo = { if (isValid) next() }),
            shape = MaterialTheme.shapes.large,
            modifier = Modifier.weight(1f).focusRequester(focus),
        )
        OutlinedTextField(
            value = port,
            onValueChange = { setPort(it.filter(Char::isDigit).take(5)) },
            label = { Text(stringResource(R.string.port)) },
            placeholder = { Text("22", style = mono) },
            singleLine = true,
            textStyle = MaterialTheme.typography.bodyLarge.merge(mono).copy(textAlign = TextAlign.Center),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number, imeAction = ImeAction.Go),
            keyboardActions = KeyboardActions(onGo = { if (isValid) next() }),
            shape = MaterialTheme.shapes.large,
            modifier = Modifier.width(96.dp),
        )
    }
    PrimaryButton(stringResource(R.string.continue_), enabled = isValid, action = next)
    LaunchedEffect(Unit) { if (server.isEmpty()) runCatching { focus.requestFocus() } }
}

@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Progress(text: String) {
    Column(
        Modifier.fillMaxWidth().padding(top = 120.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(24.dp),
    ) {
        ContainedLoadingIndicator(Modifier.size(72.dp))
        Text(text, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** A text button that copies, and says for a moment that it did. */
@Composable
private fun CopyTextButton(text: String, label: String) {
    val clipboard = LocalClipboard.current
    val haptics = LocalHapticFeedback.current
    val scope = rememberCoroutineScope()
    var isCopied by remember { mutableStateOf(false) }
    OutlinedButton(onClick = {
        scope.launch {
            clipboard.setClipEntry(ClipEntry(android.content.ClipData.newPlainText("gst", text)))
            haptics.performHapticFeedback(HapticFeedbackType.Confirm)
            isCopied = true
            kotlinx.coroutines.delay(1500)
            isCopied = false
        }
    }) {
        Icon(if (isCopied) Icons.Rounded.Check else Icons.Rounded.ContentCopy, contentDescription = null, modifier = Modifier.size(18.dp))
        Spacer(Modifier.size(8.dp))
        Text(label)
    }
}

@Composable
private fun HostKey(check: ServerCheck, trust: (ServerCheck.HostKey) -> Unit) {
    val changed = check.state == ServerCheck.State.ChangedHost
    if (changed) {
        Heading(stringResource(R.string.host_changed_title), stringResource(R.string.host_changed_text, check.server))
    } else {
        Heading(stringResource(R.string.host_unknown_title), stringResource(R.string.host_unknown_text, check.server))
    }
    val key = check.hostKey ?: return
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Caption(stringResource(R.string.fingerprint, key.type))
        CodeBlock(key.fingerprint)
    }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Caption(stringResource(R.string.see_on_server))
        val command = "ssh-keygen -lf /etc/ssh/" + hostKeyFile(key.type)
        CodeBlock(command)
        CopyTextButton(command, stringResource(R.string.copy_command))
    }
    if (changed) {
        PrimaryButton(
            stringResource(R.string.replace_key),
            colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error, contentColor = MaterialTheme.colorScheme.onError),
        ) { trust(key) }
    } else {
        PrimaryButton(stringResource(R.string.trust_server)) { trust(key) }
    }
}

@Composable
private fun Authorize(check: ServerCheck, key: String, retry: () -> Unit) {
    val context = LocalContext.current
    val command = "mkdir -p ~/.ssh && echo '$key' >> ~/.ssh/authorized_keys"
    Heading(stringResource(R.string.authorize_title), stringResource(R.string.authorize_text, check.user))
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        CodeBlock(command, maxLines = 4)
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            CopyTextButton(command, stringResource(R.string.copy_command))
            OutlinedButton(onClick = { context.share(command) }) {
                Icon(Icons.Rounded.Share, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(Modifier.size(8.dp))
                Text(stringResource(R.string.share))
            }
        }
    }
    Text(code(stringResource(R.string.authorize_note)), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    PrimaryButton(stringResource(R.string.try_again), action = retry)
}

@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
private fun Outcome(icon: ImageVector, container: Color, content: Color, title: String, text: String, done: () -> Unit) {
    Box(
        Modifier.padding(top = 32.dp).size(96.dp).background(container, MaterialShapes.Cookie9Sided.toShape()),
        contentAlignment = Alignment.Center,
    ) {
        Icon(icon, contentDescription = null, tint = content, modifier = Modifier.size(48.dp))
    }
    Heading(title, text)
    PrimaryButton(stringResource(R.string.done), action = done)
}

@Composable
private fun Waiting(reason: String?, done: () -> Unit) {
    Outcome(
        Icons.Rounded.Schedule, statusColors.warningContainer, statusColors.onWarningContainer,
        stringResource(R.string.waiting_title), stringResource(R.string.waiting_text), done,
    )
    if (reason != null) {
        CodeBlock(reason)
        if ("gst is not up" in reason || "did not answer" in reason || "the remote gst failed" in reason) {
            Text(code(stringResource(R.string.waiting_hint)), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun Failed(message: String, retry: () -> Unit, change: () -> Unit) {
    Text(stringResource(R.string.failed_title), style = MaterialTheme.typography.headlineMediumEmphasized, modifier = Modifier.padding(top = 8.dp))
    CodeBlock(message)
    PrimaryButton(stringResource(R.string.try_again), action = retry)
    TextButton(onClick = change, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.change_login)) }
}

/** The file of a server's host key of a type, as OpenSSH names them. */
private fun hostKeyFile(type: String): String = when (type) {
    "ssh-ed25519" -> "ssh_host_ed25519_key.pub"
    "ssh-rsa" -> "ssh_host_rsa_key.pub"
    else -> "ssh_host_ecdsa_key.pub"
}
