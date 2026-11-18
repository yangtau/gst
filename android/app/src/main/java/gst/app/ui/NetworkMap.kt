package gst.app.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.dp
import gst.app.Machine
import gst.app.R
import gst.app.Ring
import gst.app.Topology
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min

/**
 * The map of the network, as `gst ui` draws it: the machines on a circle,
 * this machine at the top with a ring around it, and the links between
 * them, each pointing at the machine that was linked to. This machine's
 * links take the primary color. Tapping another machine opens it, as
 * tapping it in the list does.
 *
 * The circle is in the order that gst lists the machines of [topology],
 * this machine first, in which a machine keeps its place as it goes
 * offline and comes back. While gst is off, the links are the ones that
 * the app last knew of, as the machines are: they show faint, and none
 * as this machine's, which has none then.
 */
@Composable
fun NetworkMap(topology: Topology, machines: List<Machine>, isOn: Boolean, open: (String) -> Unit, modifier: Modifier = Modifier) {
    val known = machines.associateBy { it.id }
    val shown = topology.nodes.mapNotNull { known[it.id] }
    val colors = shown.map { it.way.dotColor() }
    val plain = MaterialTheme.colorScheme.outlineVariant
    val mine = MaterialTheme.colorScheme.primary
    val name = MaterialTheme.typography.labelMedium.copy(color = MaterialTheme.colorScheme.onSurfaceVariant)
    val myName = name.copy(color = MaterialTheme.colorScheme.onSurface, fontWeight = FontWeight.SemiBold)
    val measurer = rememberTextMeasurer()
    val linksAlpha by animateFloatAsState(if (isOn) 1f else 0.45f, label = "links")
    val description = stringResource(R.string.map_description)
    val tapped by rememberUpdatedState(open)

    BoxWithConstraints(modifier.fillMaxWidth()) {
        // In dp. The circle's radius is larger with more machines, within
        // the width, which leaves room for a name on either side.
        val radius = max(48f, min(max(110f, shown.size * 13f), maxWidth.value / 2 - 96f))
        // The map reaches to its highest and lowest machines, with the
        // room that a name above or below one takes, or beside it.
        val around = Ring(shown.map { it.id }, emptyList(), Ring.Point(0f, 0f), radius).spots
        val up = around.maxOfOrNull { -it.point.y + if (it.side == Ring.Side.Above) 40f else 24f } ?: 0f
        val down = around.maxOfOrNull { it.point.y + if (it.side == Ring.Side.Below) 40f else 24f } ?: 0f
        val height = up + down
        val ring = remember(shown.map { it.id }, topology.links, maxWidth, radius) {
            Ring(shown.map { it.id }, topology.links.orEmpty(), Ring.Point(maxWidth.value / 2, up), radius)
        }
        Canvas(
            Modifier
                .fillMaxWidth()
                .height(height.dp)
                // The list below says the same to someone who does not see the map.
                .semantics { contentDescription = description }
                .pointerInput(ring, topology.self) {
                    detectTapGestures { at ->
                        ring.spots
                            .filter { it.id != topology.self }
                            .minByOrNull { hypot(it.point.x.dp.toPx() - at.x, it.point.y.dp.toPx() - at.y) }
                            ?.takeIf { hypot(it.point.x.dp.toPx() - at.x, it.point.y.dp.toPx() - at.y) <= 24.dp.toPx() }
                            ?.let { tapped(it.id) }
                    }
                },
        ) {
            fun Ring.Point.px() = Offset(x.dp.toPx(), y.dp.toPx())

            // This machine's links go last, over the others.
            for (isMine in listOf(false, true)) {
                for (line in ring.lines) {
                    if ((isOn && (line.from == topology.self || line.to == topology.self)) != isMine) continue
                    val color = if (isMine) mine else plain
                    val (start, control, end) = Triple(line.start.px(), line.control.px(), line.end.px())
                    val curve = Path().apply {
                        moveTo(start.x, start.y)
                        quadraticTo(control.x, control.y, end.x, end.y)
                    }
                    drawPath(curve, color, linksAlpha, Stroke((if (isMine) 1.75f else 1.25f).dp.toPx(), cap = StrokeCap.Round))
                    val tip = Path().apply {
                        line.tip.map { it.px() }.forEachIndexed { i, p -> if (i == 0) moveTo(p.x, p.y) else lineTo(p.x, p.y) }
                        close()
                    }
                    drawPath(tip, color, linksAlpha)
                }
            }

            ring.spots.forEachIndexed { i, spot ->
                val machine = shown[i]
                val isMe = machine.id == topology.self
                val at = spot.point.px()
                // The dot of the list: green while packets reach the machine.
                drawCircle(colors[i], 5.dp.toPx(), at)
                if (isMe) {
                    drawCircle(mine, 9.dp.toPx(), at, style = Stroke(1.5.dp.toPx()))
                }
                val text = measurer.measure(
                    if (machine.name.length > 12) machine.name.take(11) + "…" else machine.name,
                    if (isMe) myName else name, maxLines = 1,
                )
                val (wide, tall) = text.size.width.toFloat() to text.size.height.toFloat()
                val gap = 13.dp.toPx()
                drawText(
                    text,
                    topLeft = when (spot.side) {
                        Ring.Side.Right -> Offset(at.x + gap, at.y - tall / 2)
                        Ring.Side.Left -> Offset(at.x - gap - wide, at.y - tall / 2)
                        Ring.Side.Above -> Offset(at.x - wide / 2, at.y - gap - tall)
                        Ring.Side.Below -> Offset(at.x - wide / 2, at.y + 8.dp.toPx())
                    },
                )
            }
        }
    }
}
