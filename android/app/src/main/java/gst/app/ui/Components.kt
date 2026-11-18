package gst.app.ui

import android.content.ClipData
import android.content.Context
import android.content.Intent
import android.text.format.DateUtils
import android.text.format.Formatter
import androidx.compose.animation.AnimatedContent
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.KeyboardArrowRight
import androidx.compose.material.icons.outlined.ContentCopy
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.ProvideTextStyle
import androidx.compose.material3.Text
import androidx.compose.material3.SheetValue
import androidx.compose.material3.rememberBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import gst.app.Machine
import gst.app.R
import java.time.Instant
import kotlin.time.Duration
import kotlin.time.TimeSource
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** How packets reach the machine, in the words of `gst status`. */
@Composable
fun Machine.Way.text(): String = when (this) {
    Machine.Way.Here -> stringResource(R.string.way_here)
    is Machine.Way.Direct -> stringResource(R.string.way_direct)
    is Machine.Way.Relay -> if (via.isEmpty()) stringResource(R.string.way_relay) else stringResource(R.string.way_relay_via, via.joinToString(", "))
    is Machine.Way.Offline -> stringResource(R.string.way_offline)
}

/**
 * The color of the machine's dot, which says only whether packets reach
 * it: the way is for its page.
 */
@Composable
fun Machine.Way.dotColor(): Color = if (this is Machine.Way.Offline) MaterialTheme.colorScheme.outlineVariant else statusColors.good

/** A dot of a color that says how something goes, as whether packets reach a machine. */
@Composable
fun StatusDot(color: Color) {
    Box(Modifier.size(RowDot).background(color, CircleShape))
}

@Composable
fun StatusDot(way: Machine.Way) = StatusDot(way.dotColor())

/**
 * What copies [text]: whether it just did, for a moment after, as a button
 * shows it, and the action that copies.
 */
@Composable
fun rememberCopy(text: String): Pair<Boolean, () -> Unit> {
    val clipboard = LocalClipboard.current
    val haptics = LocalHapticFeedback.current
    val scope = rememberCoroutineScope()
    var isCopied by remember { mutableStateOf(false) }
    return isCopied to {
        scope.launch {
            clipboard.setClipEntry(ClipEntry(ClipData.newPlainText("gst", text)))
            haptics.performHapticFeedback(HapticFeedbackType.Confirm)
            isCopied = true
            delay(1500)
            isCopied = false
        }
    }
}

/** The icon of what copies: a check for a moment once it did. */
fun copyIcon(isCopied: Boolean): ImageVector = if (isCopied) Icons.Rounded.Check else Icons.Outlined.ContentCopy

/** Copies a text, and shows for a moment that it did. */
@Composable
fun CopyButton(text: String, label: String = stringResource(R.string.copy), modifier: Modifier = Modifier) {
    val (isCopied, copy) = rememberCopy(text)
    IconButton(onClick = copy, modifier = modifier.semantics { contentDescription = label }) {
        AnimatedContent(isCopied, label = "copied") { copied ->
            Icon(copyIcon(copied), contentDescription = null)
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
                .background(MaterialTheme.colorScheme.fill, RoundedCornerShape(14.dp))
                .padding(16.dp),
        )
    }
}

/** How wide the icon at the start of a row is. */
private val RowIcon = 28.dp

/** How wide the dot at the start of a row without an icon is. */
private val RowDot = 10.dp

/** Where the text of a row with an icon starts, and the line beneath it. */
val RowInset = PageMargin + RowIcon + 16.dp

/** Where the text of a row with only a dot starts, and the line beneath it. */
val DotRowInset = PageMargin + RowDot + 14.dp

/** A round button, in the bar at the top of a page: an icon on gray. */
@Composable
fun CircleButton(icon: ImageVector, label: String, onClick: () -> Unit, modifier: Modifier = Modifier, enabled: Boolean = true) {
    val colors = MaterialTheme.colorScheme
    Box(
        modifier
            .size(44.dp)
            .clip(CircleShape)
            .background(colors.fill)
            .border(0.5.dp, colors.hairline, CircleShape)
            .clickable(enabled = enabled, role = Role.Button, onClick = onClick)
            .semantics { contentDescription = label },
        contentAlignment = Alignment.Center,
    ) {
        Icon(icon, contentDescription = null, tint = if (enabled) colors.onSurface else colors.outline, modifier = Modifier.size(22.dp))
    }
}

/** The rows of a [Group], which it draws in order with lines between them. */
class GroupScope internal constructor() {
    internal val rows = mutableListOf<@Composable () -> Unit>()

    /** Adds a row, usually a [ListRow]. */
    fun row(content: @Composable () -> Unit) {
        rows += content
    }
}

/**
 * Rows that belong together, under a bold [header] with [action] at its
 * end, with fine lines between them that start where their text does, at
 * [inset] (past the icons of rows that have them), and beneath them a [footer] that says more of them, or what
 * went wrong when [isError].
 */
@Composable
fun Group(
    modifier: Modifier = Modifier,
    inset: Dp = PageMargin,
    header: String? = null,
    action: (@Composable () -> Unit)? = null,
    footer: String? = null,
    isError: Boolean = false,
    build: GroupScope.() -> Unit,
) {
    val rows = GroupScope().apply(build).rows
    Column(modifier.fillMaxWidth()) {
        if (header != null) {
            SectionHeader(header, action)
        }
        rows.forEachIndexed { i, row ->
            row()
            if (i < rows.lastIndex) {
                HorizontalDivider(Modifier.padding(start = inset, end = PageMargin), thickness = 0.5.dp, color = MaterialTheme.colorScheme.hairline)
            }
        }
        if (footer != null) {
            Footer(footer, isError)
        }
    }
}

/** The bold title of a group, or of a part of a page. */
val TitleStyle = TextStyle(fontSize = 22.sp, lineHeight = 28.sp, fontWeight = FontWeight.Bold)

/** The bold title over a group, with [action] at its end. */
@Composable
fun SectionHeader(text: String, action: (@Composable () -> Unit)? = null) {
    Row(
        Modifier.fillMaxWidth().heightIn(min = 44.dp).padding(start = PageMargin, end = PageMargin, top = 12.dp, bottom = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(text, style = TitleStyle, modifier = Modifier.weight(1f))
        action?.invoke()
    }
}

/** What a group says beneath it, small and gray, or red for what went wrong. */
@Composable
fun Footer(text: String, isError: Boolean = false) {
    Text(
        code(text),
        style = MaterialTheme.typography.bodySmall,
        color = if (isError) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.fillMaxWidth().padding(horizontal = PageMargin, vertical = 8.dp),
    )
}

/**
 * A row of a group: at its start a line [icon], or a [dot] of a color, or
 * neither, then the [content] (a title, and a [Subtitle] beneath it) in
 * [tint], and [trailing] at its end. With [onClick], the whole row is a
 * button, which [onLongClick] makes answer to a touch that holds, too.
 */
@Composable
fun ListRow(
    icon: ImageVector?,
    modifier: Modifier = Modifier,
    onClick: (() -> Unit)? = null,
    onLongClick: (() -> Unit)? = null,
    tint: Color = MaterialTheme.colorScheme.onSurface,
    dot: Color? = null,
    trailing: @Composable RowScope.() -> Unit = {},
    content: @Composable ColumnScope.() -> Unit,
) {
    Row(
        modifier
            .fillMaxWidth()
            .then(if (onClick != null) Modifier.combinedClickable(onLongClick = onLongClick, onClick = onClick) else Modifier)
            .heightIn(min = 56.dp)
            .padding(horizontal = PageMargin, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(if (icon == null) 14.dp else 16.dp),
    ) {
        CompositionLocalProvider(LocalContentColor provides tint) {
            when {
                icon != null -> Icon(icon, contentDescription = null, modifier = Modifier.size(RowIcon).padding(1.dp))
                dot != null -> StatusDot(dot)
            }
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                ProvideTextStyle(MaterialTheme.typography.bodyLarge.copy(fontSize = 17.sp), content = { content() })
            }
        }
        Row(verticalAlignment = Alignment.CenterVertically, content = trailing)
    }
}

/** The smaller, gray line beneath the title of a row. */
@Composable
fun Subtitle(text: String, mono: Boolean = false, color: Color = MaterialTheme.colorScheme.onSurfaceVariant) {
    Text(text, style = MaterialTheme.typography.bodyMedium, fontFamily = if (mono) FontFamily.Monospace else null, color = color)
}

/** The arrow at the end of a row that opens something. */
@Composable
fun Chevron() {
    Icon(Icons.AutoMirrored.Rounded.KeyboardArrowRight, contentDescription = null, tint = MaterialTheme.colorScheme.outline, modifier = Modifier.size(24.dp))
}

/**
 * A row that says one thing: [label], and its [value] in gray at the end,
 * after a [dot] of a color when it has one, which a button copies with
 * [copy]. With [onClick], the row is a button that opens something about
 * the value.
 */
@Composable
fun ValueRow(
    label: String,
    value: String,
    mono: Boolean = false,
    copy: Boolean = false,
    dot: Color? = null,
    onClick: (() -> Unit)? = null,
) {
    ListRow(
        null,
        onClick = onClick,
        trailing = {
            when {
                copy -> CopyButton(value, modifier = Modifier.size(36.dp))
                onClick != null -> Chevron()
            }
        },
    ) {
        // The label keeps its room; a long value gives way, in the middle.
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(label, maxLines = 1)
            Spacer(Modifier.width(16.dp))
            val text = @Composable {
                Text(
                    value,
                    style = MaterialTheme.typography.bodyLarge,
                    fontFamily = if (mono) FontFamily.Monospace else null,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.End,
                    maxLines = 1,
                    overflow = TextOverflow.MiddleEllipsis,
                )
            }
            Row(
                Modifier.weight(1f),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.End),
            ) {
                if (dot != null) StatusDot(dot)
                Box(Modifier.weight(1f, fill = false)) {
                    if (onClick == null) SelectionContainer { text() } else text()
                }
            }
        }
    }
}

/**
 * A button as wide as it may be, in the shape of a pill: filled with ink
 * when [prominent], as the one thing to do next, else with gray, in
 * [color] when it has one, as red for what removes something.
 */
@Composable
fun PillButton(
    text: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier.fillMaxWidth(),
    icon: ImageVector? = null,
    enabled: Boolean = true,
    prominent: Boolean = true,
    color: Color? = null,
) {
    val colors = MaterialTheme.colorScheme
    Button(
        onClick = onClick,
        enabled = enabled,
        shape = CircleShape,
        colors = if (prominent) {
            ButtonDefaults.buttonColors(containerColor = color ?: colors.primary, contentColor = colors.onPrimary)
        } else {
            ButtonDefaults.buttonColors(containerColor = colors.fill, contentColor = color ?: colors.onSurface)
        },
        contentPadding = PaddingValues(horizontal = 20.dp),
        modifier = modifier.heightIn(min = 52.dp),
    ) {
        if (icon != null) {
            Icon(icon, contentDescription = null, modifier = Modifier.size(20.dp))
            Spacer(Modifier.size(8.dp))
        }
        Text(text, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
    }
}

/**
 * What a screen says in its middle when it has nothing else to show: an
 * [icon], a [title], [text] that says more, and beneath them [action], the
 * one thing to do about it.
 */
@Composable
fun Notice(icon: ImageVector, title: String, text: String? = null, modifier: Modifier = Modifier, action: (@Composable () -> Unit)? = null) {
    Column(modifier.padding(horizontal = 40.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        Icon(icon, contentDescription = null, tint = MaterialTheme.colorScheme.outline, modifier = Modifier.size(56.dp))
        Spacer(Modifier.size(16.dp))
        Text(title, style = TitleStyle, textAlign = TextAlign.Center)
        if (text != null) {
            Spacer(Modifier.size(6.dp))
            Text(text, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        }
        if (action != null) {
            Spacer(Modifier.size(24.dp))
            action()
        }
    }
}

/**
 * A sheet that comes up from the bottom of the screen over what is there,
 * under its [title], and goes when someone drags it down, taps beside it,
 * or goes back, which calls [close].
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun Sheet(title: String, close: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
    ModalBottomSheet(
        onDismissRequest = close,
        // Open in full or gone: its content is short, and has no half to stop at.
        sheetState = rememberBottomSheetState(SheetValue.Hidden, setOf(SheetValue.Hidden, SheetValue.Expanded)),
        shape = RoundedCornerShape(topStart = 28.dp, topEnd = 28.dp),
        containerColor = MaterialTheme.colorScheme.surfaceContainerLow,
        contentColor = MaterialTheme.colorScheme.onSurface,
        tonalElevation = 0.dp,
    ) {
        Column(Modifier.fillMaxWidth().padding(bottom = 16.dp)) {
            Text(
                title,
                style = MaterialTheme.typography.titleMedium,
                fontWeight = FontWeight.SemiBold,
                textAlign = TextAlign.Center,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.fillMaxWidth().padding(horizontal = PageMargin).padding(bottom = 16.dp),
            )
            content()
        }
    }
}

/**
 * Does [work] every [period], counted from when each round starts, while
 * the app is on the screen and what shows [key] is.
 */
@Composable
fun RepeatWhileShown(key: Any, period: Duration, work: suspend () -> Unit) {
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

/**
 * The text with the commands that it quotes in backticks set as code: gst's
 * own words, which the app shows as they are, and the app's.
 */
@Composable
fun code(text: String): AnnotatedString {
    val background = MaterialTheme.colorScheme.fill
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
