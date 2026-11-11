package gst.app

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.DnsResolver
import android.net.VpnService
import android.os.CancellationSignal
import android.os.Handler
import android.os.Looper
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/**
 * The VPN: what the gst daemon is on other machines. gst's daemon runs in
 * it, with the links, WireGuard, and the answers for `.gst`. The system
 * makes the interface as gst says, and hands it over; gst then reads and
 * writes its packets.
 */
class GstVpnService : VpnService(), Core.System {
    companion object {
        private const val START = "gst.app.START"
        private const val STOP = "gst.app.STOP"

        /** How long gst may take to set the interface up. */
        private const val START_TIMEOUT = 30_000L

        /** How long the system's resolver may take to answer. */
        private const val RESOLVE_TIMEOUT = 5L

        /** Turns gst on; the person must have allowed the VPN (VpnService.prepare). */
        fun start(context: Context) {
            Tunnel.failure = null
            Tunnel.set(Tunnel.State.Connecting)
            Tunnel.setWanted(context, true)
            context.startService(Intent(context, GstVpnService::class.java).setAction(START))
        }

        /** Turns gst off, and keeps it off after a restart. */
        fun stop(context: Context) {
            Tunnel.setWanted(context, false)
            if (Tunnel.state.value == Tunnel.State.Off) return
            Tunnel.set(Tunnel.State.Disconnecting)
            context.startService(Intent(context, GstVpnService::class.java).setAction(STOP))
        }
    }

    /** Starts and stops gst one after another, as they block. */
    private val worker = Executors.newSingleThreadExecutor()
    private val resolver = Executors.newCachedThreadPool()
    private val main = Handler(Looper.getMainLooper())
    private var isRunning = false

    private val timeout = Runnable {
        if (Tunnel.state.value == Tunnel.State.Connecting) {
            end(getString(R.string.error_no_network))
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            STOP -> {
                end(null)
                return START_NOT_STICKY
            }
            // START, the system's own as Always-on VPN starts it, or none
            // when the system brings the service back.
            else -> begin()
        }
        return START_STICKY
    }

    override fun onRevoke() {
        // Another VPN took over, or the person turned gst off in the system's settings.
        Tunnel.setWanted(this, false)
        end(null)
    }

    override fun onDestroy() {
        main.removeCallbacks(timeout)
        if (isRunning) {
            isRunning = false
            worker.execute { Core.stop() }
            Tunnel.set(Tunnel.State.Off)
        }
        worker.shutdown()
        resolver.shutdown()
        super.onDestroy()
    }

    private fun begin() {
        if (isRunning) return
        isRunning = true
        Tunnel.set(Tunnel.State.Connecting)
        main.postDelayed(timeout, START_TIMEOUT)
        worker.execute {
            try {
                Tunnel.open(this)
                Core.start(this)
            } catch (e: Exception) {
                main.post { end(e.message ?: getString(R.string.error_no_network)) }
            }
        }
    }

    /** Stops gst, with what went wrong, if something did. */
    private fun end(failure: String?) {
        main.removeCallbacks(timeout)
        Tunnel.failure = failure
        if (isRunning) {
            isRunning = false
            Tunnel.set(Tunnel.State.Disconnecting)
            worker.execute {
                Core.stop()
                Tunnel.set(Tunnel.State.Off)
                main.post { stopSelf() }
            }
        } else {
            Tunnel.set(Tunnel.State.Off)
            stopSelf()
        }
    }

    /**
     * Sets the interface up as gst says. gst calls it on a thread of its
     * own, at the start and whenever this machine's gst IP or the
     * network's subnet changes; each time, the system makes a new
     * interface in place of the one before.
     */
    override fun apply(settings: ByteArray): Int {
        val s = GstJson.decodeFromString<InterfaceSettings>(settings.decodeToString())
        val builder = Builder()
            .setSession("gst")
            .addAddress(s.address, 32)
            .setMtu(s.mtu)
            .setMetered(false)
            // Only the names under .gst are gst's; it asks the system's own
            // resolver for the rest (forward), as Android sends every
            // query to the DNS of a VPN.
            .addDnsServer(s.dns)
            // gst's own connections, the links and the UDP paths, go
            // around the VPN that they carry.
            .addDisallowedApplication(packageName)
            .setConfigureIntent(
                PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE),
            )
        for (route in s.routes) {
            builder.addRoute(route.address, route.prefixLength)
        }
        val tunnel = builder.establish() ?: throw IllegalStateException(getString(R.string.error_not_allowed))
        main.post {
            main.removeCallbacks(timeout)
            if (isRunning) Tunnel.set(Tunnel.State.On)
        }
        return tunnel.detachFd()
    }

    /** Asks the system's resolver, on the network beneath the VPN. */
    @Suppress("DEPRECATION") // DnsResolver.getInstance, the only way before Android 17
    override fun forward(query: ByteArray): ByteArray? {
        val done = CountDownLatch(1)
        var answer: ByteArray? = null
        val cancel = CancellationSignal()
        DnsResolver.getInstance().rawQuery(
            null, query, DnsResolver.FLAG_EMPTY, resolver, cancel,
            object : DnsResolver.Callback<ByteArray> {
                override fun onAnswer(result: ByteArray, rcode: Int) {
                    answer = result
                    done.countDown()
                }

                override fun onError(error: DnsResolver.DnsException) {
                    done.countDown()
                }
            },
        )
        if (!done.await(RESOLVE_TIMEOUT, TimeUnit.SECONDS)) {
            cancel.cancel()
        }
        return answer
    }
}
