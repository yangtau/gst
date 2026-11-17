package gst.app

import kotlin.math.PI
import kotlin.math.atan2
import kotlin.math.cos
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.sin

/**
 * Where the map of the network puts the machines and the links between
 * them, as `gst ui` draws them. The machines sit on a circle, the first
 * at the top, so that a line between two of them never passes a third,
 * however the network grows. Lines are links: they bow towards the
 * center, and point at the machine that was linked to.
 *
 * The map is of [machines], by their ids, on a circle of [radius] around
 * [center]. A link of a machine that is not among them has no line.
 */
class Ring(machines: List<String>, links: List<Topology.Link>, center: Point, radius: Float) {
    data class Point(val x: Float, val y: Float)

    /** Where a machine's name goes, which is outside the circle. */
    enum class Side { Right, Left, Above, Below }

    /** A machine's place. */
    data class Spot(val id: String, val point: Point, val side: Side)

    /** A link's line, a curve from [start] by [control] to [end], and the arrowhead at its end. */
    data class Line(val from: String, val to: String, val start: Point, val control: Point, val end: Point, val tip: List<Point>)

    val spots: List<Spot> = machines.mapIndexed { index, id ->
        val angle = -PI / 2 + 2 * PI * index / machines.size
        val (cos, sin) = cos(angle).toFloat() to sin(angle).toFloat()
        val side = if (cos > 0.25f) Side.Right else if (cos < -0.25f) Side.Left else if (sin < 0) Side.Above else Side.Below
        Spot(id, Point(center.x + radius * cos, center.y + radius * sin), side)
    }

    val lines: List<Line> = run {
        val places = spots.associate { it.id to it.point }
        val drawn = mutableMapOf<Set<String>, Int>()
        links.mapNotNull { link ->
            val a = places[link.from]
            val b = places[link.to]
            if (a == null || b == null || link.from == link.to) return@mapNotNull null
            // Two machines may each link to the other: the second line
            // bows further, to the other side of the first.
            val again = drawn.getOrDefault(setOf(link.from, link.to), 0)
            drawn[setOf(link.from, link.to)] = again + 1
            val middle = Point((a.x + b.x) / 2, (a.y + b.y) / 2)
            val length = max(hypot(b.x - a.x, b.y - a.y), 1f)
            val aside = again * 16f * if (link.from < link.to) 1 else -1
            val control = Point(
                middle.x + (center.x - middle.x) * 0.3f - (b.y - a.y) / length * aside,
                middle.y + (center.y - middle.y) * 0.3f + (b.x - a.x) / length * aside,
            )
            // The line stops short of the machines, and further from the
            // one it points at, where the arrowhead goes.
            fun short(of: Point, by: Float): Point {
                val distance = max(hypot(control.x - of.x, control.y - of.y), 1f)
                return Point(of.x + (control.x - of.x) / distance * by, of.y + (control.y - of.y) / distance * by)
            }
            val start = short(a, 9f)
            val end = short(b, 12f)
            val heading = atan2(end.y - control.y, end.x - control.x)
            fun ahead(distance: Float, turned: Float) = Point(end.x + distance * cos(heading + turned), end.y + distance * sin(heading + turned))
            Line(link.from, link.to, start, control, end, listOf(ahead(3f, 0f), ahead(5f, 2.5f), ahead(5f, -2.5f)))
        }
    }
}
