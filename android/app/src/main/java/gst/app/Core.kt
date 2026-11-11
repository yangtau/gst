package gst.app

import java.io.File

/**
 * gst itself, in this process: the Go code of gstkit (`gstkit/gstkit.h`),
 * whose `jni.c` makes these methods. The app calls its "app.…" methods,
 * and [GstVpnService] runs the daemon in it, whose methods answer here too
 * while it runs. Texts cross as UTF-8 bytes.
 */
object Core {
    init {
        java.lang.System.loadLibrary("gstkit")
    }

    /** What the system does for gst's interface. gst calls it on threads of its own. */
    interface System {
        /**
         * Sets the interface up as [settings], the JSON of [InterfaceSettings],
         * say, and returns its descriptor, which gst owns from then on; it
         * throws what failed.
         */
        fun apply(settings: ByteArray): Int

        /** Asks the system's resolver a DNS query for a name outside .gst: its answer, or null. */
        fun forward(query: ByteArray): ByteArray?
    }

    /** What gst said went wrong. */
    class Failure(message: String) : Exception(message)

    /**
     * Opens gst with its files in [directory]. The first time, the machine
     * gets an identity called after [machineName], and a key.
     */
    fun open(directory: File, machineName: String) {
        open(directory.path.encodeToByteArray(), machineName.encodeToByteArray())?.let { throw Failure(it.decodeToString()) }
    }

    /** Runs the daemon in this process until [stop]. */
    fun start(system: System) {
        startDaemon(system)?.let { throw Failure(it.decodeToString()) }
    }

    /**
     * Calls a method: [request] is the JSON `{"method", "params"}`, and the
     * answer the JSON `{"result"}` or `{"error"}`. It blocks until gst
     * answers, which a call that asks another machine does after seconds.
     */
    @JvmStatic external fun call(request: ByteArray): ByteArray

    /** Stops the daemon, and returns once it has. */
    @JvmStatic external fun stop()

    @JvmStatic private external fun open(directory: ByteArray, name: ByteArray): ByteArray?

    @JvmStatic @JvmName("start") private external fun startDaemon(system: System): ByteArray?
}
