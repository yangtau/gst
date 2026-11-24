package gst.app

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant

/** What the app makes of what gst reports, and of what a person types. */
class ModelTest {
    @Test
    fun destinations() {
        assertEquals("admin@ssh.example.com", Destination.make(" admin@ssh.example.com ", ""))
        assertEquals("admin@ssh.example.com", Destination.make("admin@ssh.example.com", "22"))
        assertEquals("ssh://admin@ssh.example.com:2222", Destination.make("admin@ssh.example.com", "2222"))
        assertEquals("ssh://admin@[2001:db8::1]:2222", Destination.make("admin@2001:db8::1", "2222"))
        assertNull(Destination.make("ssh.example.com", ""))
        assertNull(Destination.make("@ssh.example.com", ""))
        assertNull(Destination.make("admin@", ""))
        assertNull(Destination.make("admin@ssh example.com", ""))
        assertNull(Destination.make("admin@ssh.example.com", "70000"))
    }

    @Test
    fun subnets() {
        val subnet = Subnet.parse("10.87.0.0/16")!!
        assertTrue(subnet.contains("10.87.255.1"))
        assertFalse(subnet.contains("10.88.0.1"))
        assertFalse(subnet.contains("not an address"))
        assertNull(Subnet.parse("10.87.0.0"))
        assertNull(Subnet.parse("10.87.0.0/33"))
    }

    @Test
    fun readsWhatGstReports() {
        // As Go writes it: nanoseconds, a zone, and null for an empty list.
        val status = GstJson.decodeFromString<DaemonStatus>(
            """{"software":"gst 0.7.0","node":{"id":"a","name":"pixel"},"links":null,
               "net":{"subnet":"10.87.0.0/16","address":"10.87.0.2","peers":[{"node":"b","name":"devbox","address":"10.87.0.3",
               "way":"direct","endpoint":"203.0.113.7:41642","handshake":"2026-11-11T17:00:00.123456789+08:00","received":5,"sent":6}]},
               "removed":{"by":{"id":"b","name":"devbox"},"at":"2026-11-11T09:00:00Z"},"unknown":1}""",
        )
        assertEquals(emptyList<DaemonStatus.Link>(), status.links)
        assertEquals(Instant.parse("2026-11-11T09:00:00.123456789Z"), status.net!!.peers[0].handshake)
        assertEquals("devbox", status.removed!!.by.name)

        val topology = GstJson.decodeFromString<Topology>(
            """{"self":"a","subnet":"10.87.0.0/16","links":[{"from":"a","to":"b"}],"nodes":[
               {"id":"a","name":"pixel","reachable":true,"route":["a"],"net":{"address":"10.87.0.2"}},
               {"id":"b","name":"devbox","reachable":true,"route":["a","b"],"net":{"address":"10.87.0.3"}},
               {"id":"c","name":"homelab","reachable":true,"route":["a","b","c"],"net":{"address":"10.86.0.4"}},
               {"id":"d","name":"attic","reachable":false,"seen":"2026-11-11T08:00:00Z"}]}""",
        )
        assertEquals(listOf(Topology.Link("a", "b")), topology.links)
        val machines = Machine.list(topology, status)
        assertEquals(listOf("pixel", "devbox", "homelab", "attic"), machines.map { it.name })
        assertEquals(Machine.Way.Here, machines[0].way)
        assertEquals(Machine.Way.Direct("203.0.113.7:41642"), machines[1].way)
        assertEquals(Machine.Way.Relay(listOf("devbox")), machines[2].way)
        // Through a relay, over UDP.
        val relayed = topology.copy(nodes = topology.nodes.map { if (it.id == "c") it.copy(via = "b") else it })
        assertEquals(Machine.Way.Relay(listOf("devbox"), udp = true), Machine.list(relayed, status)[2].way)
        // An address of another subnet is none.
        assertNull(machines[2].address)
        assertEquals(Machine.Way.Offline(Instant.parse("2026-11-11T08:00:00Z")), machines[3].way)
        // Without a running network, every other machine is offline.
        assertTrue(Machine.list(topology, null).none { it.isOnline })
    }

    /**
     * The map puts the machines on a circle, the first at the top, and
     * draws each link as a line that points at the machine linked to.
     */
    @Test
    fun ring() {
        val links = listOf("aa" to "cc", "cc" to "aa", "bb" to "aa", "aa" to "gone", "bb" to "bb").map { Topology.Link(it.first, it.second) }
        val ring = Ring(listOf("aa", "bb", "cc", "dd"), links, Ring.Point(100f, 100f), 80f)
        assertEquals(listOf("aa", "bb", "cc", "dd"), ring.spots.map { it.id })
        for ((spot, at) in ring.spots.zip(listOf(100f to 20f, 180f to 100f, 100f to 180f, 20f to 100f))) {
            assertEquals(at.first, spot.point.x, 0.001f)
            assertEquals(at.second, spot.point.y, 0.001f)
        }
        // Names go outside the circle.
        assertEquals(listOf(Ring.Side.Above, Ring.Side.Right, Ring.Side.Below, Ring.Side.Left), ring.spots.map { it.side })

        // A link to a machine that is not on the map, or to itself, has no line.
        assertEquals(listOf("aa" to "cc", "cc" to "aa", "bb" to "aa"), ring.lines.map { it.from to it.to })
        // A line leaves a little way from its machine, and ends further
        // from the one it points at, with the arrowhead before it.
        val down = ring.lines[0]
        assertEquals(100f, down.start.x, 0.001f)
        assertEquals(29f, down.start.y, 0.001f)
        assertEquals(168f, down.end.y, 0.001f)
        assertEquals(171f, down.tip[0].y, 0.001f)
        // The link back bows aside, so that both show.
        assertEquals(100f, down.control.x, 0.001f)
        assertTrue(kotlin.math.abs(ring.lines[1].control.x - 100f) > 10f)
        // A line between neighbors bows towards the center.
        val side = ring.lines[2]
        assertEquals(140f - 40f * 0.3f, side.control.x, 0.001f)
        assertEquals(60f + 40f * 0.3f, side.control.y, 0.001f)

        // What an earlier version of the app kept has no links.
        assertNull(GstJson.decodeFromString<Topology>("""{"self":"aa","subnet":"","nodes":[]}""").links)
    }

    /**
     * The chart of the pings draws them from one side to the other, leaves
     * a gap where no answer came, and keeps the longest below its top.
     */
    @Test
    fun pings() {
        assertEquals(emptyList<Pings.Line>(), Pings().lines)
        assertEquals(0..1, Pings().span)
        assertEquals(100.0, Pings().scale.top, 0.0)

        val direct = Machine.Way.Direct("203.0.113.7:41642")
        var pings = Pings()
            .answered(58.0, Machine.Way.Relay(listOf("devbox")))
            .answered(61.0, Machine.Way.Relay(listOf("devbox")))
            .answered(24.0, direct)
            .failed("no answer")
            .answered(22.0, direct)
        // One line whatever way the pings took, until one got no answer.
        assertEquals(listOf(listOf(0, 1, 2), listOf(4)), pings.lines.map { line -> line.points.map { it.id } })
        assertEquals(0..4, pings.span)
        assertEquals(direct, pings.latest.last().way)
        assertEquals("no answer", pings.latest[3].error)

        for ((longest, step, top) in listOf(Triple(61.0, 50.0, 100.0), Triple(23.0, 10.0, 30.0), Triple(218.0, 100.0, 300.0), Triple(320.0, 200.0, 400.0), Triple(1.2, 1.0, 2.0), Triple(0.3, 1.0, 2.0))) {
            val scale = Pings().answered(longest, direct).scale
            assertEquals("$longest", step, scale.step, 0.0)
            assertEquals("$longest", top, scale.top, 0.0)
        }

        // Only the latest stay, and the chart moves along with them.
        repeat(Pings.MOST + 10) { pings = pings.answered(20.0, direct) }
        assertEquals(Pings.MOST, pings.latest.size)
        assertEquals(15..(Pings.MOST + 14), pings.span)
        assertEquals(15, pings.latest.first().id)

        assertEquals("1.4 ms", Pings.text(1.44))
        assertEquals("24 ms", Pings.text(23.6))
    }

    @Test
    fun settings() {
        val settings = GstJson.decodeFromString<InterfaceSettings>(
            """{"address":"10.87.0.2","routes":[{"address":"10.87.0.0","mask":"255.255.0.0"},{"address":"198.18.0.53","mask":"255.255.255.255"}],
               "dns":"198.18.0.53","domain":"gst","mtu":1280}""",
        )
        assertEquals(listOf(16, 32), settings.routes.map { it.prefixLength })
    }
}
