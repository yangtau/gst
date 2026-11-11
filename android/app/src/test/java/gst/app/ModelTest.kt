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
            """{"self":"a","subnet":"10.87.0.0/16","nodes":[
               {"id":"a","name":"pixel","reachable":true,"route":["a"],"net":{"address":"10.87.0.2"}},
               {"id":"b","name":"devbox","reachable":true,"route":["a","b"],"net":{"address":"10.87.0.3"}},
               {"id":"c","name":"homelab","reachable":true,"route":["a","b","c"],"net":{"address":"10.86.0.4"}},
               {"id":"d","name":"attic","reachable":false,"seen":"2026-11-11T08:00:00Z"}]}""",
        )
        val machines = Machine.list(topology, status)
        assertEquals(listOf("pixel", "devbox", "homelab", "attic"), machines.map { it.name })
        assertEquals(Machine.Way.Here, machines[0].way)
        assertEquals(Machine.Way.Direct("203.0.113.7:41642"), machines[1].way)
        assertEquals(Machine.Way.Relay(listOf("devbox")), machines[2].way)
        // An address of another subnet is none.
        assertNull(machines[2].address)
        assertEquals(Machine.Way.Offline(Instant.parse("2026-11-11T08:00:00Z")), machines[3].way)
        // Without a running network, every other machine is offline.
        assertTrue(Machine.list(topology, null).none { it.isOnline })
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
