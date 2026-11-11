package gst.app

import android.app.Application

/** The app's process, which keeps one model for every activity. */
class GstApplication : Application() {
    val model: AppModel by lazy { AppModel(this) }
}
