package gst.app.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.dp
import gst.app.Machine
import gst.app.Pings
import java.util.Locale

/**
 * The pings of a machine while its sheet is open: the way that the latest
 * took and its time, and a chart of the times of the latest ones, each
 * line in the color of its way. Until a ping comes back, the way is
 * [way], the one that gst reports to the machine.
 */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
fun PingView(pings: Pings, way: Machine.Way, modifier: Modifier = Modifier) {
    val last = pings.latest.lastOrNull()
    val shown = last?.way ?: way
    Column(
        modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.fill, RoundedCornerShape(20.dp))
            .padding(start = 20.dp, top = 18.dp, end = 16.dp, bottom = 16.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Row(verticalAlignment = Alignment.Top, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    StatusDot(shown)
                    Text(shown.text(), style = MaterialTheme.typography.titleMediumEmphasized)
                }
                val error = last?.error
                if (error != null) {
                    Text(error.sentence, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
                } else if (shown is Machine.Way.Direct && shown.endpoint != null) {
                    SelectionContainer {
                        Text(
                            shown.endpoint,
                            style = MaterialTheme.typography.bodySmall,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
            Text(
                last?.time?.let(Pings::text) ?: "–",
                style = MaterialTheme.typography.headlineLarge,
                fontWeight = FontWeight.SemiBold,
                color = if (last?.time == null) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.onSurface,
            )
        }
        PingChart(pings, Modifier.fillMaxWidth().height(148.dp))
    }
}

/** The chart: time upwards, the latest ping at the right. */
@Composable
private fun PingChart(pings: Pings, modifier: Modifier = Modifier) {
    val direct = Machine.Way.Direct(null).color()
    val relay = Machine.Way.Relay(emptyList()).color()
    val lost = MaterialTheme.colorScheme.error
    val grid = MaterialTheme.colorScheme.outlineVariant
    val label = MaterialTheme.typography.labelSmall.copy(color = MaterialTheme.colorScheme.onSurfaceVariant)
    val measurer = rememberTextMeasurer()
    Canvas(modifier) {
        val scale = pings.scale
        val span = pings.span
        // The times stand at the right, each on its line; a dot at either end stays inside.
        val left = 5.dp.toPx()
        val right = size.width - 52.dp.toPx()
        val top = 8.dp.toPx()
        val bottom = size.height - 8.dp.toPx()
        fun x(id: Int) = left + (right - left) * (id - span.first) / (span.last - span.first)
        fun y(time: Double) = bottom - (bottom - top) * (time / scale.top).toFloat()

        var time = 0.0
        while (time <= scale.top) {
            drawLine(grid, Offset(0f, y(time)), Offset(right + 8.dp.toPx(), y(time)), strokeWidth = 1f)
            val text = measurer.measure(String.format(Locale.ROOT, "%.0f ms", time), label)
            drawText(text, topLeft = Offset(size.width - text.size.width, y(time) - text.size.height / 2))
            time += scale.step
        }

        val stroke = Stroke(2.5.dp.toPx(), cap = StrokeCap.Round, join = StrokeJoin.Round)
        for (line in pings.lines) {
            val color = if (line.isDirect) direct else relay
            val points = line.points.filter { it.id >= span.first }.map { Offset(x(it.id), y(it.time)) }
            when (points.size) {
                0 -> {}
                // A ping between two without an answer.
                1 -> drawCircle(color, 2.dp.toPx(), points[0])
                else -> drawPath(curve(points), color, style = stroke)
            }
        }
        // A ping without an answer leaves its mark on the ground.
        val arm = 3.5.dp.toPx()
        for (ping in pings.latest.filter { it.time == null && it.id >= span.first }) {
            val at = Offset(x(ping.id), y(0.0))
            drawLine(lost, at - Offset(arm, arm), at + Offset(arm, arm), strokeWidth = 2.dp.toPx(), cap = StrokeCap.Round)
            drawLine(lost, at + Offset(-arm, arm), at + Offset(arm, -arm), strokeWidth = 2.dp.toPx(), cap = StrokeCap.Round)
        }
        val last = pings.latest.lastOrNull()
        val answered = last?.time
        if (last != null && answered != null) {
            drawCircle(if (last.isDirect) direct else relay, 4.5.dp.toPx(), Offset(x(last.id), y(answered)))
        }
    }
}

/**
 * A smooth line through [points], which stand evenly apart: it rises and
 * falls only where they do, and so never dips under one of them.
 */
private fun curve(points: List<Offset>): Path {
    val slopes = points.zipWithNext { a, b -> (b.y - a.y) / (b.x - a.x) }
    val tangents = points.indices.map { i ->
        when {
            i == 0 -> slopes.first()
            i == points.lastIndex -> slopes.last()
            slopes[i - 1] * slopes[i] <= 0f -> 0f
            else -> 2 * slopes[i - 1] * slopes[i] / (slopes[i - 1] + slopes[i])
        }
    }
    return Path().apply {
        moveTo(points[0].x, points[0].y)
        for (i in 0 until points.lastIndex) {
            val a = points[i]
            val b = points[i + 1]
            val third = (b.x - a.x) / 3
            cubicTo(a.x + third, a.y + tangents[i] * third, b.x - third, b.y - tangents[i + 1] * third, b.x, b.y)
        }
    }
}
