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

// gst's colors: the blue of its icon, far from the green, amber, and red
// that say how things go, for a phone without colors of its own (before
// Android 12), and for the pictures of the screens.

private val Light = lightColorScheme(
    primary = Color(0xFF2F52DC), onPrimary = Color.White,
    primaryContainer = Color(0xFFDDE2FF), onPrimaryContainer = Color(0xFF1A37A8),
    secondary = Color(0xFF595E72), onSecondary = Color.White,
    secondaryContainer = Color(0xFFDEE1F9), onSecondaryContainer = Color(0xFF414659),
    tertiary = Color(0xFF7A5298), onTertiary = Color.White,
    tertiaryContainer = Color(0xFFF2DAFF), onTertiaryContainer = Color(0xFF603A7E),
    error = Color(0xFFBA1A1A), onError = Color.White,
    errorContainer = Color(0xFFFFDAD6), onErrorContainer = Color(0xFF93000A),
    background = Color(0xFFF8F9FD), onBackground = Color(0xFF191B22),
    surface = Color(0xFFF8F9FD), onSurface = Color(0xFF191B22),
    surfaceVariant = Color(0xFFE1E2EC), onSurfaceVariant = Color(0xFF44464F),
    outline = Color(0xFF757780), outlineVariant = Color(0xFFC5C6D0),
    inverseSurface = Color(0xFF2E3037), inverseOnSurface = Color(0xFFF0F0F8), inversePrimary = Color(0xFFB7C4FF),
    surfaceDim = Color(0xFFD8D9E1), surfaceBright = Color(0xFFF8F9FD),
    surfaceContainerLowest = Color.White, surfaceContainerLow = Color(0xFFF2F3FA),
    surfaceContainer = Color(0xFFECEDF5), surfaceContainerHigh = Color(0xFFE6E7EF),
    surfaceContainerHighest = Color(0xFFE1E2E9),
)

private val Dark = darkColorScheme(
    primary = Color(0xFFB7C4FF), onPrimary = Color(0xFF0A2A8C),
    primaryContainer = Color(0xFF1A3AAE), onPrimaryContainer = Color(0xFFDDE2FF),
    secondary = Color(0xFFC1C5DD), onSecondary = Color(0xFF2B3042),
    secondaryContainer = Color(0xFF414659), onSecondaryContainer = Color(0xFFDEE1F9),
    tertiary = Color(0xFFE2B7FF), onTertiary = Color(0xFF482366),
    tertiaryContainer = Color(0xFF603A7E), onTertiaryContainer = Color(0xFFF2DAFF),
    error = Color(0xFFFFB4AB), onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A), onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF111318), onBackground = Color(0xFFE2E2EA),
    surface = Color(0xFF111318), onSurface = Color(0xFFE2E2EA),
    surfaceVariant = Color(0xFF44464F), onSurfaceVariant = Color(0xFFC5C6D0),
    outline = Color(0xFF8F909A), outlineVariant = Color(0xFF44464F),
    inverseSurface = Color(0xFFE2E2EA), inverseOnSurface = Color(0xFF2E3037), inversePrimary = Color(0xFF2F52DC),
    surfaceDim = Color(0xFF111318), surfaceBright = Color(0xFF37393F),
    surfaceContainerLowest = Color(0xFF0C0E13), surfaceContainerLow = Color(0xFF191B22),
    surfaceContainer = Color(0xFF1D1F26), surfaceContainerHigh = Color(0xFF282A30),
    surfaceContainerHighest = Color(0xFF33353B),
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
