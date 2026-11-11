package gst.app

import java.time.Instant

/**
 * A machine of the network, as the app shows it: what the topology says
 * of it, and what the virtual network knows of the packets to it.
 */
data class Machine(
    val id: String,
    val name: String,
    val address: String?,
    val software: String?,
    val way: Way,
    val handshake: Instant? = null,
    val received: Long = 0,
    val sent: Long = 0,
) {
    /** How packets reach a machine now. */
    sealed interface Way {
        /** It is this machine. */
        data object Here : Way

        /** Over UDP, to the address given. */
        data class Direct(val endpoint: String?) : Way

        /** Along the SSH links, through the machines named. */
        data class Relay(val via: List<String>) : Way

        /** No way leads to it now; it was last known online then. */
        data class Offline(val seen: Instant?) : Way
    }

    val isHere: Boolean get() = way == Way.Here
    val isOnline: Boolean get() = way !is Way.Offline

    /** The name that every program on the network reaches it by. */
    val hostname: String get() = "$name.gst"

    /** The id as gst status shows it. */
    val shortID: String get() = id.take(8)

    companion object {
        /**
         * The machines of [topology]: this machine, then the ones that
         * packets reach, then the rest, each group by name. [status] adds
         * what only a running network knows; without it, as while gst is
         * off, every other machine is offline.
         */
        fun list(topology: Topology, status: DaemonStatus?): List<Machine> {
            val names = topology.nodes.reversed().associate { it.id to it.name }
            val peers = (status?.net?.peers ?: emptyList()).reversed().associateBy { it.node }
            val subnet = Subnet.parse(topology.subnet)
            val machines = topology.nodes.map { node ->
                val peer = peers[node.id]
                // An address of another subnet is one that the machine has
                // yet to leave: nothing reaches it there.
                val address = (node.net?.address ?: peer?.address)?.takeUnless { subnet?.contains(it) == false }
                val way: Way = when {
                    node.id == topology.self -> if (status == null) Way.Offline(null) else Way.Here
                    status == null -> Way.Offline(node.seen)
                    node.direct == true || peer?.way == "direct" -> Way.Direct(peer?.endpoint)
                    // The route runs from this machine to the machine; what
                    // lies between is what the packets pass.
                    node.reachable -> Way.Relay((node.route ?: emptyList()).drop(1).dropLast(1).map { names[it] ?: it.take(8) })
                    else -> Way.Offline(node.seen)
                }
                Machine(
                    id = node.id,
                    name = node.name,
                    address = address,
                    software = node.software,
                    way = way,
                    handshake = peer?.handshake,
                    received = peer?.received ?: 0,
                    sent = peer?.sent ?: 0,
                )
            }
            return machines.sortedWith(
                compareByDescending<Machine> { it.id == topology.self }
                    .thenByDescending { it.isOnline }
                    .thenBy { it.name }
                    .thenBy { it.id },
            )
        }
    }
}

/** An IPv4 prefix, to tell which addresses are the network's. */
class Subnet private constructor(private val base: Long, private val mask: Long) {
    fun contains(address: String): Boolean {
        val number = number(address) ?: return false
        return number and mask == base
    }

    companion object {
        fun parse(text: String): Subnet? {
            val parts = text.split("/")
            if (parts.size != 2) return null
            val bits = parts[1].toIntOrNull()?.takeIf { it in 0..32 } ?: return null
            val address = number(parts[0]) ?: return null
            val mask = if (bits == 0) 0L else (0xFFFFFFFFL shl (32 - bits)) and 0xFFFFFFFFL
            return Subnet(address and mask, mask)
        }

        private fun number(address: String): Long? {
            val parts = address.split(".")
            if (parts.size != 4) return null
            var number = 0L
            for (part in parts) {
                val byte = part.toIntOrNull()?.takeIf { it in 0..255 && part.all(Char::isDigit) } ?: return null
                number = (number shl 8) or byte.toLong()
            }
            return number
        }
    }
}

/**
 * An SSH destination as a person types it on a phone: who and where, and
 * a port when it is not 22.
 */
object Destination {
    /**
     * The destination that gst takes for [server] ("user@host") and [port],
     * or null while they do not make one.
     */
    fun make(server: String, port: String): String? {
        val server = server.trim()
        val port = port.trim()
        val at = server.lastIndexOf('@')
        if (at <= 0) return null
        val host = server.substring(at + 1)
        if (host.isEmpty() || server.contains(' ')) return null
        if (port.isEmpty() || port == "22") return server
        val number = port.toIntOrNull()?.takeIf { it in 1..65535 && port.all(Char::isDigit) } ?: return null
        // A bare IPv6 address needs brackets to keep its colons from the port's.
        val bracketed = if (host.contains(':') && !host.startsWith("[")) "[$host]" else host
        return "ssh://${server.substring(0, at)}@$bracketed:$number"
    }
}
