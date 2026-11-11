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

// gst's colors: the teal of its icon, for a phone without colors of its
// own (before Android 12), and for the pictures of the screens.

private val Light = lightColorScheme(
    primary = Color(0xFF006B5F), onPrimary = Color.White,
    primaryContainer = Color(0xFF9EF2E2), onPrimaryContainer = Color(0xFF005047),
    secondary = Color(0xFF4A635E), onSecondary = Color.White,
    secondaryContainer = Color(0xFFCCE8E1), onSecondaryContainer = Color(0xFF334B46),
    tertiary = Color(0xFF456179), onTertiary = Color.White,
    tertiaryContainer = Color(0xFFCCE5FF), onTertiaryContainer = Color(0xFF2D4960),
    error = Color(0xFFBA1A1A), onError = Color.White,
    errorContainer = Color(0xFFFFDAD6), onErrorContainer = Color(0xFF93000A),
    background = Color(0xFFF4FBF8), onBackground = Color(0xFF171D1B),
    surface = Color(0xFFF4FBF8), onSurface = Color(0xFF171D1B),
    surfaceVariant = Color(0xFFDAE5E1), onSurfaceVariant = Color(0xFF3F4946),
    outline = Color(0xFF6F7976), outlineVariant = Color(0xFFBEC9C5),
    inverseSurface = Color(0xFF2B3230), inverseOnSurface = Color(0xFFECF2EF), inversePrimary = Color(0xFF82D5C6),
    surfaceDim = Color(0xFFD5DBD9), surfaceBright = Color(0xFFF4FBF8),
    surfaceContainerLowest = Color.White, surfaceContainerLow = Color(0xFFEEF5F2),
    surfaceContainer = Color(0xFFE9EFEC), surfaceContainerHigh = Color(0xFFE3EAE7),
    surfaceContainerHighest = Color(0xFFDDE4E1),
)

private val Dark = darkColorScheme(
    primary = Color(0xFF82D5C6), onPrimary = Color(0xFF003731),
    primaryContainer = Color(0xFF005047), onPrimaryContainer = Color(0xFF9EF2E2),
    secondary = Color(0xFFB1CCC5), onSecondary = Color(0xFF1C3530),
    secondaryContainer = Color(0xFF334B46), onSecondaryContainer = Color(0xFFCCE8E1),
    tertiary = Color(0xFFADCAE5), onTertiary = Color(0xFF153349),
    tertiaryContainer = Color(0xFF2D4960), onTertiaryContainer = Color(0xFFCCE5FF),
    error = Color(0xFFFFB4AB), onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A), onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF0E1513), onBackground = Color(0xFFDDE4E1),
    surface = Color(0xFF0E1513), onSurface = Color(0xFFDDE4E1),
    surfaceVariant = Color(0xFF3F4946), onSurfaceVariant = Color(0xFFBEC9C5),
    outline = Color(0xFF899390), outlineVariant = Color(0xFF3F4946),
    inverseSurface = Color(0xFFDDE4E1), inverseOnSurface = Color(0xFF2B3230), inversePrimary = Color(0xFF006B5F),
    surfaceDim = Color(0xFF0E1513), surfaceBright = Color(0xFF343B39),
    surfaceContainerLowest = Color(0xFF090F0E), surfaceContainerLow = Color(0xFF171D1B),
    surfaceContainer = Color(0xFF1B211F), surfaceContainerHigh = Color(0xFF252B2A),
    surfaceContainerHighest = Color(0xFF303634),
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
    good = Color(0xFF1B873F), goodContainer = Color(0xFFC4EFCB), onGoodContainer = Color(0xFF00531F),
    warning = Color(0xFFB45309), warningContainer = Color(0xFFFFE0C2), onWarningContainer = Color(0xFF6B3000),
)

private val DarkStatus = StatusColors(
    good = Color(0xFF6DD58C), goodContainer = Color(0xFF00531F), onGoodContainer = Color(0xFFC4EFCB),
    warning = Color(0xFFFFB870), warningContainer = Color(0xFF5A2A00), onWarningContainer = Color(0xFFFFE0C2),
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
