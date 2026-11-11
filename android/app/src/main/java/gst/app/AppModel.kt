package gst.app

import android.content.Context
import android.content.Intent
import android.net.VpnService
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import java.time.Instant

/**
 * What the app shows and does: this machine, its links, and the network
 * as gst's daemon reports it while gst is on.
 *
 * The app is to gst here what the gst command is elsewhere. What it
 * changes (the name, the links) goes into the configuration through gst's
 * "app.…" methods, and the daemon then reloads it; what it shows comes
 * from the daemon, which runs in [GstVpnService].
 */
class AppModel private constructor(
    private val context: Context?,
    /** The VPN's state; the demo's own for a made-up network. */
    val tunnel: StateFlow<Tunnel.State>,
    private val gst: Gst,
) {
    constructor(context: Context) : this(context.applicationContext, Tunnel.state, Gst())

    /** This machine and its configuration; null until gst is open. */
    var info by mutableStateOf<AppInfo?>(null)
        private set

    /** What the daemon reports; null while gst is off. */
    var status by mutableStateOf<DaemonStatus?>(null)
        private set

    /**
     * The network as the daemon last reported it, kept while gst is off
     * and between launches, so that the machines stay on the screen.
     */
    var topology by mutableStateOf<Topology?>(null)
        private set

    /** Why gst could not be opened, which nothing in the app gets past. */
    var failure by mutableStateOf<String?>(null)
        private set

    /**
     * Asks the person to allow gst's VPN with the system's dialog, and says
     * whether they did; the activity sets it.
     */
    var askConsent: (suspend (Intent) -> Boolean)? = null

    /** The model shows a made-up network, and asks gst nothing; see Demo. */
    private var isDemo = false

    val isOn: Boolean get() = tunnel.value == Tunnel.State.On

    /** The machines of the network, this machine first. */
    fun machines(isOn: Boolean = this.isOn): List<Machine> {
        val topology = topology ?: return emptyList()
        return Machine.list(topology, if (isOn) status else null)
    }

    /** This machine's gst IP, while it has one. */
    fun address(isOn: Boolean = this.isOn): String? = if (isOn) status?.net?.address else null

    /** What keeps this machine from the network, or from part of it: as `Problems:` in `gst status`. */
    sealed interface Problem {
        data class Removed(val by: String, val at: Instant) : Problem
        data class Network(val error: String, val hint: String?) : Problem
        data class Link(val destination: String, val error: String) : Problem
    }

    fun problems(isOn: Boolean = this.isOn): List<Problem> {
        val status = status?.takeIf { isOn } ?: return emptyList()
        return buildList {
            status.removed?.let { add(Problem.Removed(it.by.name, it.at)) }
            status.net?.error?.let { add(Problem.Network(it, status.net.hint)) }
            for (link in status.links) {
                if (link.state == DaemonStatus.Link.State.Down) add(Problem.Link(link.destination, link.error ?: ""))
            }
        }
    }

    /** Opens gst. Called once, when the app starts. */
    suspend fun start() {
        val context = context ?: return
        if (isDemo || info != null) return
        topology = context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE).getString(TOPOLOGY, null)
            ?.let { runCatching { GstJson.decodeFromString<Topology>(it) }.getOrNull() }
        try {
            withContext(Dispatchers.IO) { Tunnel.open(context) }
            info = gst.call("app.info")
        } catch (e: Throwable) {
            failure = e.message ?: e.toString()
            return
        }
        refresh()
    }

    /** Asks the daemon how things are. The app calls it every few seconds while it is on the screen. */
    suspend fun refresh() {
        val context = context ?: return
        if (isDemo || failure != null) return
        if (!isOn) {
            status = null
            return
        }
        // The daemon also writes to the configuration: the machine that a
        // link reached, and the links to machines that were removed.
        runCatching { gst.call<AppInfo>("app.info") }.onSuccess { info = it }
        val status = runCatching { gst.call<DaemonStatus>("daemon.status") }.getOrNull() ?: return
        val topology = runCatching { gst.call<Topology>("mesh.topology") }.getOrNull() ?: return
        this.status = status
        if (topology != this.topology) {
            this.topology = topology
            context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE).edit()
                .putString(TOPOLOGY, GstJson.encodeToString(Topology.serializer(), topology)).apply()
        }
    }

    /** Turns gst on, and returns once it is; the first time, the system asks the person to allow it. */
    suspend fun turnOn() {
        val context = context ?: return
        VpnService.prepare(context)?.let { consent ->
            if (askConsent?.invoke(consent) != true) {
                throw GstError("failed", context.getString(R.string.error_declined))
            }
        }
        GstVpnService.start(context)
        val state = withTimeoutOrNull(40_000) {
            tunnel.first { it == Tunnel.State.On || it == Tunnel.State.Off }
        }
        if (state != Tunnel.State.On) {
            throw GstError("failed", Tunnel.failure ?: context.getString(R.string.error_no_network))
        }
        refresh()
    }

    fun turnOff() {
        val context = context ?: return
        GstVpnService.stop(context)
        status = null
    }

    // What the gst command does elsewhere.

    /** `gst name <name>`. */
    suspend fun rename(name: String) {
        info = gst.call("app.name", params(NameParams(name)))
        reload()
    }

    /** Logs in to the server at [destination] once, and says how that went. */
    suspend fun check(destination: String): ServerCheck = gst.call("app.check", params(LinkParams(destination)))

    /** Records the host key that a person checked. */
    suspend fun trust(key: ServerCheck.HostKey, destination: String) {
        gst.run("app.trust", params(TrustParams(destination, key.key)))
    }

    /** `gst add <destination>`: adds the link, and turns gst on, as the link joins this machine to a network only while gst runs. */
    suspend fun add(destination: String) {
        info = gst.call("app.add", params(LinkParams(destination)))
        if (tunnel.value == Tunnel.State.Off) {
            turnOn()
        } else {
            reload()
        }
    }

    /** How a link that was just added goes. */
    sealed interface LinkOutcome {
        /** It runs; the machine at its other end is called this. */
        data class Running(val name: String) : LinkOutcome

        /** It failed, as gst says; it tries again by itself. */
        data class Failed(val error: String) : LinkOutcome

        /** It is still connecting. */
        data object Pending : LinkOutcome
    }

    /** Watches the link to [destination] for a while, until it shows how it goes. */
    suspend fun watchLink(destination: String): LinkOutcome {
        val deadline = System.currentTimeMillis() + 20_000
        var isReloaded = false
        while (System.currentTimeMillis() < deadline) {
            // A daemon that was starting while the link was added may
            // have read the configuration before it.
            if (isOn && !isReloaded) {
                runCatching { gst.run("daemon.reload") }
                isReloaded = true
            }
            refresh()
            status?.links?.firstOrNull { it.destination == destination }?.let { link ->
                if (link.state == DaemonStatus.Link.State.Running) return LinkOutcome.Running(link.peer?.name ?: destination)
                if (!link.error.isNullOrEmpty()) return LinkOutcome.Failed(link.error)
            }
            delay(300)
        }
        return LinkOutcome.Pending
    }

    /** `gst unlink <destination>`. */
    suspend fun unlink(destination: String) {
        info = gst.call("app.unlink", params(LinkParams(destination)))
        reload()
    }

    /** `gst ping <machine>`, once. */
    suspend fun ping(machine: Machine): PingReply = gst.call("net.ping", params(NodeParams(machine.id)))

    /** `gst remove <machine>`. */
    suspend fun remove(machine: Machine): Removal {
        val removal: Removal = gst.call("mesh.remove", params(NodeParams(machine.id)))
        refresh()
        return removal
    }

    /** Replaces this machine's key with a new one. Servers accept it once it is in their authorized_keys. */
    suspend fun newKey() {
        info = gst.call("app.new_key")
    }

    /** The end of the daemon's log. */
    suspend fun log(): String {
        if (isDemo) return ""
        return gst.call<DaemonLog>("app.log").text
    }

    /** Makes the daemon take up what the app changed. */
    private suspend fun reload() {
        if (isOn) runCatching { gst.run("daemon.reload") }
        refresh()
    }

    companion object {
        private const val PREFERENCES = "network"
        private const val TOPOLOGY = "topology"

        /** A model that shows what it is given and asks gst nothing, for previews and screenshots; see Demo. */
        fun demo(info: AppInfo, topology: Topology?, status: DaemonStatus?): AppModel =
            AppModel(
                null,
                MutableStateFlow(if (status == null) Tunnel.State.Off else Tunnel.State.On),
                Gst { throw GstError("failed", "This network is made up.") },
            ).apply {
                isDemo = true
                this.info = info
                this.topology = topology
                this.status = status
            }
    }
}
