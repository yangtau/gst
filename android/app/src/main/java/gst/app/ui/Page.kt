package gst.app.ui

import androidx.compose.foundation.ScrollState
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.asPaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.RectangleShape
import androidx.compose.ui.graphics.Shape
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.chrisbanes.haze.HazeInput
import dev.chrisbanes.haze.HazeState
import dev.chrisbanes.haze.blur.HazeBlurStyle
import dev.chrisbanes.haze.blur.HazeColorEffect
import dev.chrisbanes.haze.blur.hazeBlur
import dev.chrisbanes.haze.hazeSource
import dev.chrisbanes.haze.rememberHazeState

/** How far the content of a page stays from its sides. */
val PageMargin = 20.dp

/** The height of the bar at the top of a page, beneath the status bar. */
private val BarHeight = 56.dp

/** What scrolls on the page, which the glass over it blurs. */
private val LocalGlass = staticCompositionLocalOf<HazeState?> { null }

/**
 * Frosted glass in [shape]: what scrolls beneath shows through it, blurred
 * and lightened, as a sheet of matte glass would show it, with a fine line
 * around its edge; [outline] leaves the line out. Where the phone cannot
 * blur, it is the page's color, nearly opaque.
 */
@Composable
fun Modifier.glass(shape: Shape, outline: Boolean = true): Modifier {
    val page = MaterialTheme.colorScheme.background
    val state = LocalGlass.current
    val clipped = clip(shape)
    val frosted = if (state == null) {
        clipped.background(page.copy(alpha = 0.94f))
    } else {
        clipped.hazeBlur(
            HazeInput.Sources(state),
            HazeBlurStyle {
                blurRadius(28.dp)
                noiseFactor(0.05f)
                backgroundColor(page)
                colorEffects(listOf(HazeColorEffect.tint(page.copy(alpha = 0.62f))))
                fallbackColorEffect(HazeColorEffect.tint(page.copy(alpha = 0.94f)))
            },
        )
    }
    return if (outline) frosted.border(0.5.dp, MaterialTheme.colorScheme.hairline, shape) else frosted
}

/**
 * A page: a large [title], the content beneath it, which scrolls, and a bar
 * at the top with [navigation] at its start and [actions] at its end, which
 * turns to glass and shows the title small once the large one scrolls away.
 * [subtitle] goes beneath the title, and [floating] floats over the bottom
 * of the page, in glass, with [bottomSpace] kept free beneath the content
 * so that its end can scroll out from under it.
 */
@Composable
fun Page(
    title: String,
    modifier: Modifier = Modifier,
    navigation: (@Composable () -> Unit)? = null,
    actions: @Composable RowScope.() -> Unit = {},
    subtitle: (@Composable () -> Unit)? = null,
    scroll: ScrollState = rememberScrollState(),
    bottomSpace: Dp = 0.dp,
    floating: (@Composable BoxScope.() -> Unit)? = null,
    content: @Composable ColumnScope.() -> Unit,
) {
    val glass = rememberHazeState()
    val density = LocalDensity.current
    val colors = MaterialTheme.colorScheme
    CompositionLocalProvider(LocalGlass provides glass) {
        Box(modifier.fillMaxSize().background(colors.background)) {
            Column(
                Modifier
                    .fillMaxSize()
                    .hazeSource(glass)
                    .verticalScroll(scroll)
                    .padding(top = WindowInsets.statusBars.asPaddingValues().calculateTopPadding() + BarHeight)
                    .padding(bottom = WindowInsets.navigationBars.asPaddingValues().calculateBottomPadding() + bottomSpace + 24.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                Column(Modifier.widthIn(max = 640.dp).fillMaxWidth()) {
                    Column(Modifier.padding(horizontal = PageMargin).padding(bottom = 16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        LargeTitle(title)
                        subtitle?.invoke()
                    }
                    content()
                }
            }

            // The bar: clear over the title, glass once content scrolls beneath it.
            val hairline = colors.hairline
            Box(Modifier.fillMaxWidth()) {
                Box(
                    Modifier
                        .matchParentSize()
                        .graphicsLayer { alpha = (scroll.value / with(density) { 24.dp.toPx() }).coerceIn(0f, 1f) }
                        .glass(RectangleShape, outline = false)
                        .drawBehind { drawLine(hairline, Offset(0f, size.height), Offset(size.width, size.height), 0.5.dp.toPx()) },
                )
                Row(
                    Modifier.statusBarsPadding().height(BarHeight).fillMaxWidth().padding(horizontal = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(10.dp),
                ) {
                    navigation?.invoke()
                    // The title, small, in the room the buttons leave.
                    Text(
                        title,
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.SemiBold,
                        textAlign = TextAlign.Center,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier
                            .weight(1f)
                            .graphicsLayer {
                                alpha = ((scroll.value - with(density) { 32.dp.toPx() }) / with(density) { 16.dp.toPx() }).coerceIn(0f, 1f)
                            },
                    )
                    actions()
                }
            }
            floating?.invoke(this)
        }
    }
}

/** The title of a page, large and bold. */
@Composable
private fun LargeTitle(text: String) {
    Text(
        text,
        fontSize = 34.sp,
        lineHeight = 41.sp,
        fontWeight = FontWeight.Bold,
        maxLines = 1,
        overflow = TextOverflow.Ellipsis,
    )
}
