package gst.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import gst.app.AppModel
import gst.app.R
import kotlinx.coroutines.delay

/** The end of gst's log: what the links did, without the names of servers, which gst keeps out of it. */
@Composable
fun LogScreen(model: AppModel, close: () -> Unit) {
    val context = LocalContext.current
    var text by remember { mutableStateOf("") }
    val scroll = rememberScrollState()
    BackHandler { close() }
    LaunchedEffect(Unit) {
        while (true) {
            runCatching { model.log() }.onSuccess { text = it }
            delay(2000)
        }
    }
    LaunchedEffect(text) { scroll.scrollTo(scroll.maxValue) }
    Page(
        title = stringResource(R.string.log),
        navigation = { CircleButton(Icons.AutoMirrored.Rounded.ArrowBack, stringResource(R.string.back), close) },
        actions = {
            CircleButton(Icons.Outlined.Share, stringResource(R.string.share), { context.share(text) }, enabled = text.isNotEmpty())
        },
        scroll = scroll,
    ) {
        SelectionContainer {
            Text(
                text.ifEmpty { stringResource(R.string.log_empty) },
                style = MaterialTheme.typography.bodySmall,
                fontFamily = FontFamily.Monospace,
                modifier = Modifier.padding(horizontal = PageMargin),
            )
        }
    }
}
