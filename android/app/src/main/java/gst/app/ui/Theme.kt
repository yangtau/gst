package gst.app.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.MaterialExpressiveTheme
import androidx.compose.material3.MotionScheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color

// gst's colors: the ink of its icon on a white page, or a black one in the
// dark, and grays, beside which the green, amber, and red that say how
// things go stand out. The phone's own colors would tint all of it.

private val Light = lightColorScheme(
    primary = Color(0xFF1C1C1F), onPrimary = Color.White,
    primaryContainer = Color(0xFFEDEDF0), onPrimaryContainer = Color(0xFF1C1C1F),
    secondary = Color(0xFF5D5E66), onSecondary = Color.White,
    secondaryContainer = Color(0xFFEDEDF0), onSecondaryContainer = Color(0xFF45464E),
    tertiary = Color(0xFF128A4B), onTertiary = Color.White,
    tertiaryContainer = Color(0xFFC5EFD2), onTertiaryContainer = Color(0xFF00522A),
    error = Color(0xFFD70015), onError = Color.White,
    errorContainer = Color(0xFFFFDAD6), onErrorContainer = Color(0xFF93000A),
    background = Color.White, onBackground = Color(0xFF1B1B1F),
    surface = Color.White, onSurface = Color(0xFF1B1B1F),
    surfaceVariant = Color(0xFFEDEDF0), onSurfaceVariant = Color(0xFF8A8A8E),
    outline = Color(0xFFAEAEB2), outlineVariant = Color(0xFFDEDEE3),
    inverseSurface = Color(0xFF303034), inverseOnSurface = Color(0xFFF2F0F5), inversePrimary = Color(0xFFE6E6EA),
    surfaceDim = Color(0xFFDEDEE3), surfaceBright = Color.White,
    surfaceContainerLowest = Color.White, surfaceContainerLow = Color.White,
    surfaceContainer = Color(0xFFF5F5F7), surfaceContainerHigh = Color(0xFFF0F0F3),
    surfaceContainerHighest = Color(0xFFE8E8EC),
)

private val Dark = darkColorScheme(
    primary = Color(0xFFF2F2F5), onPrimary = Color(0xFF1C1C1F),
    primaryContainer = Color(0xFF2C2C2E), onPrimaryContainer = Color(0xFFF2F2F5),
    secondary = Color(0xFFC7C6CE), onSecondary = Color(0xFF2F3036),
    secondaryContainer = Color(0xFF2C2C2E), onSecondaryContainer = Color(0xFFE3E2EA),
    tertiary = Color(0xFF4FCE8E), onTertiary = Color(0xFF00391C),
    tertiaryContainer = Color(0xFF00522A), onTertiaryContainer = Color(0xFFC5EFD2),
    error = Color(0xFFFF6961), onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A), onErrorContainer = Color(0xFFFFDAD6),
    background = Color.Black, onBackground = Color(0xFFF2F2F5),
    surface = Color.Black, onSurface = Color(0xFFF2F2F5),
    surfaceVariant = Color(0xFF2C2C2E), onSurfaceVariant = Color(0xFF98989F),
    outline = Color(0xFF6C6C70), outlineVariant = Color(0xFF38383A),
    inverseSurface = Color(0xFFE4E3E8), inverseOnSurface = Color(0xFF303034), inversePrimary = Color(0xFF1C1C1F),
    surfaceDim = Color.Black, surfaceBright = Color(0xFF3A3A3C),
    surfaceContainerLowest = Color.Black, surfaceContainerLow = Color(0xFF1C1C1E),
    surfaceContainer = Color(0xFF242426), surfaceContainerHigh = Color(0xFF2C2C2E),
    surfaceContainerHighest = Color(0xFF3A3A3C),
)

/** The colors that say how things go, which the scheme has no roles for. */
@Immutable
data class StatusColors(
    /** Packets reach the machine, or the link runs. */
    val good: Color,
    val goodContainer: Color,
    val onGoodContainer: Color,
    /** Something keeps part of the network away. */
    val warning: Color,
    val warningContainer: Color,
    val onWarningContainer: Color,
)

private val LightStatus = StatusColors(
    good = Color(0xFF1FA84F), goodContainer = Color(0xFFD9F5E2), onGoodContainer = Color(0xFF00522A),
    warning = Color(0xFFC77700), warningContainer = Color(0xFFFFF1DC), onWarningContainer = Color(0xFF6B3000),
)

private val DarkStatus = StatusColors(
    good = Color(0xFF30D158), goodContainer = Color(0xFF0E3B1E), onGoodContainer = Color(0xFFC5EFD2),
    warning = Color(0xFFFF9F0A), warningContainer = Color(0xFF3A2400), onWarningContainer = Color(0xFFFFE2C4),
)

val LocalStatusColors = staticCompositionLocalOf { LightStatus }

/** gst in its own colors, light or dark as the phone is. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
fun GstTheme(dark: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    CompositionLocalProvider(LocalStatusColors provides if (dark) DarkStatus else LightStatus) {
        MaterialExpressiveTheme(colorScheme = if (dark) Dark else Light, motionScheme = MotionScheme.expressive(), content = content)
    }
}

val statusColors: StatusColors
    @Composable get() = LocalStatusColors.current

/** What buttons, cards, and blocks of code are filled with, a gray a little apart from the page. */
val ColorScheme.fill: Color
    get() = surfaceContainerHigh

/** A sheet that opens over the page: white, or in the dark a gray above the black. */
val ColorScheme.sheet: Color
    get() = surfaceContainerLow

/** The lines between the rows of a group, and around glass. */
val ColorScheme.hairline: Color
    get() = outlineVariant
