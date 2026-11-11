package gst.app

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.VpnService

/** Turns gst on again after a restart, or an update of the app, if a person left it on. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED && intent.action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        if (Tunnel.isWanted(context) && VpnService.prepare(context) == null) {
            GstVpnService.start(context)
        }
    }
}
