package gst.app.ui

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.MaterialExpressiveTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MotionScheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.material3.ListItemColors
import androidx.compose.material3.ListItemDefaults
import androidx.compose.ui.platform.LocalContext

// gst's colors: the ink of its icon and grays, beside which the green,
// amber, and red that say how things go stand out, for a phone without
// colors of its own (before Android 12), and for the pictures of the
// screens.

private val Light = lightColorScheme(
    primary = Color(0xFF1C1C1F), onPrimary = Color.White,
    primaryContainer = Color(0xFFE4E4E9), onPrimaryContainer = Color(0xFF1C1C1F),
    secondary = Color(0xFF5D5E66), onSecondary = Color.White,
    secondaryContainer = Color(0xFFE3E3EA), onSecondaryContainer = Color(0xFF45464E),
    tertiary = Color(0xFF128A4B), onTertiary = Color.White,
    tertiaryContainer = Color(0xFFC5EFD2), onTertiaryContainer = Color(0xFF00522A),
    error = Color(0xFFBA1A1A), onError = Color.White,
    errorContainer = Color(0xFFFFDAD6), onErrorContainer = Color(0xFF93000A),
    background = Color(0xFFF9F9FB), onBackground = Color(0xFF1B1B1F),
    surface = Color(0xFFF9F9FB), onSurface = Color(0xFF1B1B1F),
    surfaceVariant = Color(0xFFE3E2E9), onSurfaceVariant = Color(0xFF46464D),
    outline = Color(0xFF76767E), outlineVariant = Color(0xFFC7C6CE),
    inverseSurface = Color(0xFF303034), inverseOnSurface = Color(0xFFF2F0F5), inversePrimary = Color(0xFFE6E6EA),
    surfaceDim = Color(0xFFD9D9DE), surfaceBright = Color(0xFFF9F9FB),
    surfaceContainerLowest = Color.White, surfaceContainerLow = Color(0xFFF3F3F6),
    surfaceContainer = Color(0xFFEDEDF1), surfaceContainerHigh = Color(0xFFE8E8EC),
    surfaceContainerHighest = Color(0xFFE2E2E6),
)

private val Dark = darkColorScheme(
    primary = Color(0xFFE6E6EA), onPrimary = Color(0xFF1C1C1F),
    primaryContainer = Color(0xFF3A3A40), onPrimaryContainer = Color(0xFFE6E6EA),
    secondary = Color(0xFFC7C6CE), onSecondary = Color(0xFF2F3036),
    secondaryContainer = Color(0xFF45464D), onSecondaryContainer = Color(0xFFE3E2EA),
    tertiary = Color(0xFF4FCE8E), onTertiary = Color(0xFF00391C),
    tertiaryContainer = Color(0xFF00522A), onTertiaryContainer = Color(0xFFC5EFD2),
    error = Color(0xFFFFB4AB), onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A), onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF121215), onBackground = Color(0xFFE4E3E8),
    surface = Color(0xFF121215), onSurface = Color(0xFFE4E3E8),
    surfaceVariant = Color(0xFF46464D), onSurfaceVariant = Color(0xFFC7C6CE),
    outline = Color(0xFF909098), outlineVariant = Color(0xFF46464D),
    inverseSurface = Color(0xFFE4E3E8), inverseOnSurface = Color(0xFF303034), inversePrimary = Color(0xFF1C1C1F),
    surfaceDim = Color(0xFF121215), surfaceBright = Color(0xFF38383D),
    surfaceContainerLowest = Color(0xFF0D0D10), surfaceContainerLow = Color(0xFF1A1A1E),
    surfaceContainer = Color(0xFF1E1E22), surfaceContainerHigh = Color(0xFF29292D),
    surfaceContainerHighest = Color(0xFF343438),
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
    good = Color(0xFF128A4B), goodContainer = Color(0xFFC5EFD2), onGoodContainer = Color(0xFF00522A),
    warning = Color(0xFFA15C00), warningContainer = Color(0xFFFFE2C4), onWarningContainer = Color(0xFF6B3000),
)

private val DarkStatus = StatusColors(
    good = Color(0xFF4FCE8E), goodContainer = Color(0xFF00522A), onGoodContainer = Color(0xFFC5EFD2),
    warning = Color(0xFFE5A640), warningContainer = Color(0xFF532A00), onWarningContainer = Color(0xFFFFE2C4),
)

val LocalStatusColors = staticCompositionLocalOf { LightStatus }

/** gst in Material 3 Expressive, in the phone's own colors where it has them. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
fun GstTheme(dark: Boolean = isSystemInDarkTheme(), brandColors: Boolean = false, content: @Composable () -> Unit) {
    val context = LocalContext.current
    val colors: ColorScheme = when {
        !brandColors && Build.VERSION.SDK_INT >= Build.VERSION_CODES.S ->
            if (dark) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        dark -> Dark
        else -> Light
    }
    CompositionLocalProvider(LocalStatusColors provides if (dark) DarkStatus else LightStatus) {
        MaterialExpressiveTheme(colorScheme = colors, motionScheme = MotionScheme.expressive(), content = content)
    }
}

val statusColors: StatusColors
    @Composable get() = LocalStatusColors.current

/** The background of a page of groups, as Android's own settings have it. */
val ColorScheme.page: Color
    get() = if (surface.luminance() > 0.5f) surfaceContainer else surface

/** The background of an item of a group, which stands out of the page. */
val ColorScheme.item: Color
    get() = if (surface.luminance() > 0.5f) surfaceContainerLowest else surfaceContainerHigh

/** The colors of an item of a group. */
@Composable
fun itemColors(): ListItemColors = ListItemDefaults.segmentedColors(containerColor = MaterialTheme.colorScheme.item)
