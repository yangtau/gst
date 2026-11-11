package gst.app.ui

import android.content.ClipData
import android.content.Context
import android.content.Intent
import android.text.format.DateUtils
import android.text.format.Formatter
import androidx.compose.animation.AnimatedContent
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import gst.app.Machine
import gst.app.R
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.time.Instant

/** How packets reach the machine, in the words of `gst status`. */
@Composable
fun Machine.Way.text(): String = when (this) {
    Machine.Way.Here -> stringResource(R.string.way_here)
    is Machine.Way.Direct -> stringResource(R.string.way_direct)
    is Machine.Way.Relay -> if (via.isEmpty()) stringResource(R.string.way_relay) else stringResource(R.string.way_relay_via, via.joinToString(", "))
    is Machine.Way.Offline -> stringResource(R.string.way_offline)
}

@Composable
fun Machine.Way.color(): Color = when (this) {
    Machine.Way.Here, is Machine.Way.Direct -> statusColors.good
    is Machine.Way.Relay -> MaterialTheme.colorScheme.tertiary
    is Machine.Way.Offline -> MaterialTheme.colorScheme.outline
}

/** A dot that says whether packets reach a machine: filled while they do. */
@Composable
fun StatusDot(color: Color, filled: Boolean = true, size: Int = 10) {
    val shape = CircleShape
    Box(
        Modifier
            .size(size.dp)
            .then(if (filled) Modifier.background(color, shape) else Modifier.border(1.5.dp, color, shape)),
    )
}

@Composable
fun StatusDot(way: Machine.Way) = StatusDot(way.color(), filled = way !is Machine.Way.Offline)

/** Copies a text, and shows for a moment that it did. */
@Composable
fun CopyButton(text: String, label: String = stringResource(R.string.copy), modifier: Modifier = Modifier) {
    val clipboard = LocalClipboard.current
    val haptics = LocalHapticFeedback.current
    val scope = rememberCoroutineScope()
    var isCopied by remember { mutableStateOf(false) }
    IconButton(
        onClick = {
            scope.launch {
                clipboard.setClipEntry(ClipEntry(ClipData.newPlainText("gst", text)))
                haptics.performHapticFeedback(HapticFeedbackType.Confirm)
                isCopied = true
                delay(1500)
                isCopied = false
            }
        },
        modifier = modifier.semantics { contentDescription = label },
    ) {
        AnimatedContent(isCopied, label = "copied") { copied ->
            Icon(if (copied) Icons.Rounded.Check else Icons.Rounded.ContentCopy, contentDescription = null)
        }
    }
}

/** A text set apart for a person to compare or copy: a key, a fingerprint, a command. */
@Composable
fun CodeBlock(text: String, modifier: Modifier = Modifier, maxLines: Int = Int.MAX_VALUE) {
    SelectionContainer(modifier) {
        Text(
            text,
            style = MaterialTheme.typography.bodyMedium,
            fontFamily = FontFamily.Monospace,
            maxLines = maxLines,
            overflow = if (maxLines == Int.MAX_VALUE) TextOverflow.Clip else TextOverflow.MiddleEllipsis,
            modifier = Modifier
                .fillMaxWidth()
                .background(MaterialTheme.colorScheme.surfaceContainerHighest, RoundedCornerShape(16.dp))
                .padding(16.dp),
        )
    }
}

/** Items that belong together, as Material lists them now: each its own piece, a little apart. */
@Composable
fun Segmented(modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    Column(modifier, verticalArrangement = Arrangement.spacedBy(ListItemDefaults.SegmentedGap), content = content)
}

/** The title over a group of items. */
@Composable
fun SectionTitle(text: String, modifier: Modifier = Modifier) {
    Text(
        text,
        style = MaterialTheme.typography.titleSmall,
        color = MaterialTheme.colorScheme.primary,
        modifier = modifier.padding(start = 16.dp, top = 8.dp, bottom = 8.dp),
    )
}

/**
 * The text with the commands that it quotes in backticks set as code: gst's
 * own words, which the app shows as they are, and the app's.
 */
@Composable
fun code(text: String): AnnotatedString {
    val background = MaterialTheme.colorScheme.surfaceContainerHighest
    return buildAnnotatedString {
        text.split('`').forEachIndexed { i, part ->
            if (i % 2 == 1) {
                withStyle(SpanStyle(fontFamily = FontFamily.Monospace, background = background)) { append(part) }
            } else {
                append(part)
            }
        }
    }
}

/** The text as a sentence starts: gst's own words start small, as errors do. */
val String.sentence: String
    get() = replaceFirstChar { it.uppercase() }

/** How long ago it was, as "3 hours ago". */
fun Instant.ago(now: Long = System.currentTimeMillis()): String =
    DateUtils.getRelativeTimeSpanString(toEpochMilli().coerceAtMost(now), now, DateUtils.SECOND_IN_MILLIS).toString()

/** A number of bytes, as "1.2 MB". */
fun Context.bytes(count: Long): String = Formatter.formatFileSize(this, count)

/** Offers a text to other apps, as the system's share sheet does. */
fun Context.share(text: String) {
    val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
    startActivity(Intent.createChooser(send, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
}
