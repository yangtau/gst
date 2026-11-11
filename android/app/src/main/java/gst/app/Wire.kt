package gst.app

import kotlinx.serialization.ExperimentalSerializationApi
import kotlinx.serialization.KSerializer
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.descriptors.PrimitiveKind
import kotlinx.serialization.descriptors.PrimitiveSerialDescriptor
import kotlinx.serialization.encoding.Decoder
import kotlinx.serialization.encoding.Encoder
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonNamingStrategy
import java.time.Instant
import java.time.OffsetDateTime

// What gst reports, as its JSON says it: the types of internal/wire and
// internal/mobile that the app reads. Keys are snake_case there and
// camelCase here; GstJson converts them.

@OptIn(ExperimentalSerializationApi::class)
val GstJson = Json {
    namingStrategy = JsonNamingStrategy.SnakeCase
    ignoreUnknownKeys = true
    // Go writes an empty list as null.
    coerceInputValues = true
    explicitNulls = false
}

/** A time as Go writes it: RFC 3339, with any number of digits of a second's fraction. */
object TimeSerializer : KSerializer<Instant> {
    override val descriptor = PrimitiveSerialDescriptor("gst.Time", PrimitiveKind.STRING)
    override fun serialize(encoder: Encoder, value: Instant) = encoder.encodeString(value.toString())
    override fun deserialize(decoder: Decoder): Instant = OffsetDateTime.parse(decoder.decodeString()).toInstant()
}

typealias Time = @Serializable(with = TimeSerializer::class) Instant

/** Who a machine is. */
@Serializable
data class NodeInfo(val id: String, val name: String, val software: String? = null)

/** This machine and what its configuration holds ("app.info"). */
@Serializable
data class AppInfo(
    val node: NodeInfo,
    /** The line for a server's `authorized_keys` that lets this machine log in. */
    val publicKey: String,
    val links: List<Link> = emptyList(),
) {
    @Serializable
    data class Link(val destination: String, val target: String? = null)
}

/** How a login to a server went ("app.check"). */
@Serializable
data class ServerCheck(
    val destination: String,
    val server: String,
    val user: String,
    val state: State,
    val hostKey: HostKey? = null,
    val error: String? = null,
) {
    @Serializable
    enum class State {
        @SerialName("ready") Ready,
        @SerialName("unknown_host") UnknownHost,
        @SerialName("changed_host") ChangedHost,
        @SerialName("unauthorized") Unauthorized,
        @SerialName("failed") Failed,
    }

    @Serializable
    data class HostKey(val type: String, val fingerprint: String, val key: String)
}

/** The end of the daemon's log ("app.log"). */
@Serializable
data class DaemonLog(val text: String = "")

/** The daemon and what it runs ("daemon.status"). */
@Serializable
data class DaemonStatus(
    val software: String = "",
    val node: NodeInfo,
    val links: List<Link> = emptyList(),
    val net: Network? = null,
    val removed: Removed? = null,
) {
    @Serializable
    data class Link(
        val destination: String,
        val state: State,
        val since: Time? = null,
        val error: String? = null,
        val reconnects: Int = 0,
        val peer: NodeInfo? = null,
    ) {
        @Serializable
        enum class State {
            @SerialName("connecting") Connecting,
            @SerialName("running") Running,
            @SerialName("down") Down,
        }
    }

    @Serializable
    data class Network(
        val subnet: String? = null,
        val address: String? = null,
        val `interface`: String? = null,
        val error: String? = null,
        val hint: String? = null,
        val peers: List<Peer> = emptyList(),
    ) {
        @Serializable
        data class Peer(
            val node: String,
            val name: String,
            val address: String,
            val way: String = "",
            val endpoint: String? = null,
            val handshake: Time? = null,
            val received: Long = 0,
            val sent: Long = 0,
        )
    }

    @Serializable
    data class Removed(val by: NodeInfo, val at: Time)
}

/** The network as this machine knows it ("mesh.topology"). */
@Serializable
data class Topology(
    /** This machine's id. */
    val self: String,
    val subnet: String = "",
    val nodes: List<Node> = emptyList(),
) {
    @Serializable
    data class Node(
        val id: String,
        val name: String,
        val software: String? = null,
        val reachable: Boolean = false,
        val route: List<String>? = null,
        val direct: Boolean? = null,
        val seen: Time? = null,
        val net: Net? = null,
    ) {
        @Serializable
        data class Net(val address: String)
    }
}

/** A packet to a machine and back ("net.ping"). */
@Serializable
data class PingReply(
    val node: NodeInfo,
    val address: String,
    val way: String? = null,
    val endpoint: String? = null,
    val via: List<NodeInfo> = emptyList(),
    val timeMs: Double,
)

/** What removing a machine did ("mesh.remove"). */
@Serializable
data class Removal(
    val node: NodeInfo,
    /** It was this machine, which left the network. */
    val self: Boolean = false,
    val stranded: List<NodeInfo> = emptyList(),
)

/** What the system's interface should be, as gst says it. */
@Serializable
data class InterfaceSettings(
    /** This machine's gst IP. */
    val address: String,
    /** What goes to the interface: the subnet of the network, and [dns]. */
    val routes: List<Route>,
    /** The address that answers for the names under [domain]. */
    val dns: String,
    val domain: String,
    val mtu: Int,
) {
    @Serializable
    data class Route(val address: String, val mask: String) {
        /** The length of the prefix that [mask] says. */
        val prefixLength: Int
            get() = mask.split(".").sumOf { Integer.bitCount(it.toInt()) }
    }
}

// The params of the methods.

@Serializable data class NodeParams(val node: String)
@Serializable data class LinkParams(val destination: String)
@Serializable data class NameParams(val name: String)
@Serializable data class TrustParams(val destination: String, val key: String)
