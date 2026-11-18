package gst.app

import java.util.Locale
import kotlin.math.ceil
import kotlin.math.floor
import kotlin.math.log10
import kotlin.math.max
import kotlin.math.pow

/**
 * The pings of one machine, sent a second apart while the sheet of its
 * pings is open: what the chart of their times draws. [latest] are the
 * latest, oldest first.
 */
data class Pings(val latest: List<Ping> = emptyList(), private val sent: Int = 0) {
    /** A packet to the machine and back. */
    data class Ping(
        /** Its place among the pings sent. */
        val id: Int,
        /** How long it took there and back, in milliseconds; null when no answer came. */
        val time: Double?,
        /** The way it took, when an answer came. */
        val way: Machine.Way?,
        /** Why no answer came, in gst's words. */
        val error: String?,
    )

    /** A line of the chart: pings that came back one after another. [id] is the first. */
    data class Line(val id: Int, val points: List<Point>)

    /** An answered ping on a line. */
    data class Point(val id: Int, val time: Double)

    /** The time at the top of the chart, and the step between the times that it marks below. */
    data class Scale(val step: Double, val top: Double)

    /** A ping came back after [time] milliseconds by [way]. */
    fun answered(time: Double, way: Machine.Way): Pings = add(Ping(sent, time, way, null))

    /** A ping got no answer, for the reason that gst gives. */
    fun failed(error: String): Pings = add(Ping(sent, null, null, error))

    private fun add(ping: Ping) = Pings((latest + ping).takeLast(MOST), sent + 1)

    /**
     * The pings that the chart shows, by their ids, from one side of it to
     * the other: far apart while they are few, and closer as more come.
     */
    val span: IntRange
        get() {
            val first = latest.firstOrNull()?.id ?: 0
            return first..max(latest.lastOrNull()?.id ?: 0, first + 1)
        }

    /** The chart's lines: a ping without an answer leaves a gap. */
    val lines: List<Line>
        get() {
            val lines = mutableListOf<Line>()
            var isOpen = false
            for (ping in latest) {
                val time = ping.time
                if (time == null) {
                    isOpen = false
                    continue
                }
                val point = Point(ping.id, time)
                if (isOpen) {
                    lines[lines.lastIndex] = lines.last().let { it.copy(points = it.points + point) }
                } else {
                    lines += Line(ping.id, listOf(point))
                }
                isOpen = true
            }
            return lines
        }

    /**
     * The chart's scale. The step is 1, 2, or 5 times a power of ten and
     * never under a millisecond, and the longest ping stays below the top.
     */
    val scale: Scale
        get() {
            val longest = latest.mapNotNull { it.time }.maxOrNull() ?: return Scale(50.0, 100.0)
            val rough = max(longest, 1.0) / 3
            val power = 10.0.pow(floor(log10(rough)))
            val step = max(1.0, (listOf(1, 2, 5).firstOrNull { it * power >= rough } ?: 10) * power)
            return Scale(step, step * max(2.0, ceil(longest * 1.1 / step)))
        }

    companion object {
        /** How many pings the chart shows at most: the latest, five minutes of them. */
        const val MOST = 300

        /** A time as the app writes it: "23 ms", and "1.4 ms" under ten. */
        fun text(time: Double): String = String.format(Locale.ROOT, if (time < 9.95) "%.1f ms" else "%.0f ms", time)
    }
}
