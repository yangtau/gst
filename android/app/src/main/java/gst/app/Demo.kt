package gst.app

import android.content.Intent
import gst.app.ui.AddStep
import gst.app.ui.Route
import java.time.Instant

/**
 * The app on a made-up network, without gst: what the tests of the screens
 * show (src/test), and what a debug build opens with
 * `adb shell am start -n <app>/gst.app.MainActivity --es GstDemo <screen>`, and
 * `--es GstDemoAppearance dark` for the dark appearance.
 */
enum class Demo(val screen: String) {
    /** The first screen of a phone in a network of five. */
    Home("home"),

    /** The same, with what `Problems:` in `gst status` would list. */
    Problems("problems"),

    /** A phone that has joined no network yet. */
    Welcome("welcome"),

    /** A phone in a network, with gst off. */
    Off("off"),

    /** Adding a machine: the server shows a host key for the first time. */
    HostKey("host-key"),

    /** Adding a machine: the server does not accept the phone's key yet. */
    Authorize("authorize"),

    /** One machine. */
    Machine("machine"),
    Settings("settings"),
    ;

    fun model(now: Instant = Instant.now()): AppModel = when (this) {
        Welcome -> AppModel.demo(AppInfo(phone, publicKey, emptyList()), null, null)
        Off -> AppModel.demo(info, topology(now), null)
        Problems -> AppModel.demo(info, topology(now), status(now, broken = true))
        else -> AppModel.demo(info, topology(now), status(now, broken = false))
    }

    val route: Route
        get() = when (this) {
            HostKey, Authorize -> Route.Add
            Settings -> Route.Settings
            else -> Route.Home
        }

    val machine: String? get() = if (this == Machine) id("b1") else null

    val addStep: AddStep
        get() = when (this) {
            HostKey -> AddStep.HostKey(
                ServerCheck(
                    destination, "ssh.example.com:22", "admin", ServerCheck.State.UnknownHost,
                    ServerCheck.HostKey("ssh-ed25519", "SHA256:Hq3uT9cLw0mVxE2yRk5bZpN8sFaJd7oGi1PhWnXtYvA", "ssh-ed25519 AAAA"),
                ),
            )
            Authorize -> AddStep.Authorize(ServerCheck(destination, "ssh.example.com:22", "admin", ServerCheck.State.Unauthorized))
            else -> AddStep.Form
        }

    companion object {
        /** The screen that an intent asks for. */
        fun from(intent: Intent?): Demo? {
            val screen = intent?.getStringExtra("GstDemo") ?: return null
            return entries.firstOrNull { it.screen == screen }
        }

        // The network: a phone that links to devbox, and through it reaches the rest.

        private fun id(prefix: String) = prefix + "0".repeat(32 - prefix.length)

        private const val SOFTWARE = "gst 0.7.0+a2ffc87"
        private const val destination = "admin@ssh.example.com"
        private val phone = NodeInfo(id("4f"), "pixel", SOFTWARE)
        private const val publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFJtRmWpNA4ljYiNsI30z0faYkrWoE85zrKftgc8Ar3 gst@pixel"

        private val info = AppInfo(
            phone, publicKey,
            listOf(AppInfo.Link(destination, id("b1")), AppInfo.Link("ssh://deploy@192.0.2.20:2222", id("c2"))),
        )

        private fun topology(now: Instant): Topology {
            fun node(prefix: String, name: String, address: String, route: List<String>? = null, direct: Boolean = false, seen: Long? = null) =
                Topology.Node(
                    id(prefix), name, SOFTWARE, reachable = route != null, route = route?.map(::id), direct = direct,
                    seen = seen?.let { now.minusSeconds(it) }, net = Topology.Node.Net(address),
                )
            return Topology(
                id("4f"), "10.87.0.0/16",
                listOf(
                    node("4f", "pixel", "10.87.26.14", route = listOf("4f")),
                    node("b1", "devbox", "10.87.61.7", route = listOf("4f", "b1"), direct = true),
                    node("c2", "homelab", "10.87.140.3", route = listOf("4f", "b1", "c2")),
                    node("d3", "laptop", "10.87.26.1", route = listOf("4f", "b1", "d3"), direct = true),
                    node("e4", "attic", "10.87.9.44", seen = 3 * 3600),
                ),
            )
        }

        private fun status(now: Instant, broken: Boolean) = DaemonStatus(
            SOFTWARE, phone,
            listOf(
                DaemonStatus.Link(destination, DaemonStatus.Link.State.Running, now.minusSeconds(5400), peer = NodeInfo(id("b1"), "devbox")),
                if (broken) {
                    DaemonStatus.Link("ssh://deploy@192.0.2.20:2222", DaemonStatus.Link.State.Down, now.minusSeconds(90), "TCP connection failed", 4)
                } else {
                    DaemonStatus.Link("ssh://deploy@192.0.2.20:2222", DaemonStatus.Link.State.Running, now.minusSeconds(600), reconnects = 1, peer = NodeInfo(id("c2"), "homelab"))
                },
            ),
            DaemonStatus.Network(
                "10.87.0.0/16", "10.87.26.14", "tun0",
                error = if (broken) "the subnet 10.87.0.0/16 overlaps 10.87.3.0/24 on this machine" else null,
                hint = if (broken) "choose another subnet with `gst subnet <cidr>`" else null,
                peers = listOf(
                    DaemonStatus.Network.Peer(id("b1"), "devbox", "10.87.61.7", "direct", "203.0.113.7:41642", now.minusSeconds(12), 50_540_000, 3_250_000),
                    DaemonStatus.Network.Peer(id("c2"), "homelab", "10.87.140.3", "relay", null, now.minusSeconds(64), 1_204_000, 380_000),
                    DaemonStatus.Network.Peer(id("d3"), "laptop", "10.87.26.1", "direct", "192.168.1.23:15355", now.minusSeconds(31), 912_000, 77_000),
                ),
            ),
        )
    }
}
