package gst.app

import android.content.Context
import android.provider.Settings
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.File

/**
 * gst's VPN, as [GstVpnService] runs it: off, or on with gst's daemon in
 * this process. Turning it on is `gst up`, and it stays on: the app
 * starts it again after a restart (see [BootReceiver]), until a person
 * turns it off.
 */
object Tunnel {
    enum class State { Off, Connecting, On, Disconnecting }

    private val mutableState = MutableStateFlow(State.Off)
    val state: StateFlow<State> = mutableState.asStateFlow()

    /** Why the VPN last ended without being asked to, if it did. */
    @Volatile var failure: String? = null
        internal set

    internal fun set(state: State) {
        mutableState.value = state
    }

    private const val PREFERENCES = "tunnel"
    private const val WANTED = "on"

    /** Whether a person left gst on, which it then is again after a restart. */
    fun isWanted(context: Context): Boolean =
        context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE).getBoolean(WANTED, false)

    internal fun setWanted(context: Context, wanted: Boolean) {
        context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE).edit().putBoolean(WANTED, wanted).apply()
    }

    private var isOpen = false

    /**
     * Opens gst, once in this process. Its files are in a directory that
     * stays out of backups: restored onto another phone, the machine's
     * identity and key would make the two one machine to the network.
     */
    @Synchronized
    fun open(context: Context) {
        if (isOpen) return
        val directory = File(context.noBackupFilesDir, "gst").apply { mkdirs() }
        val name = Settings.Global.getString(context.contentResolver, Settings.Global.DEVICE_NAME) ?: android.os.Build.MODEL
        Core.open(directory, name)
        isOpen = true
    }
}
