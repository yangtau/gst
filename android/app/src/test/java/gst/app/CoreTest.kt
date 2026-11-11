package gst.app

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.BeforeClass
import org.junit.Test
import java.nio.file.Files

/**
 * gst's Go code through the JNI of gstkit, as the app calls it: built for
 * this machine by `build-core.sh host`, without which these tests are
 * skipped.
 */
class CoreTest {
    companion object {
        private var isLoaded = false

        @BeforeClass
        @JvmStatic
        fun open() {
            isLoaded = runCatching { Core }.isSuccess
            if (isLoaded) Core.open(Files.createTempDirectory("gst").toFile(), "Test Phone")
        }
    }

    private fun call(method: String, params: String = "{}"): JsonObject =
        GstJson.parseToJsonElement(Core.call("""{"method":"$method","params":$params}""".encodeToByteArray()).decodeToString()).jsonObject

    @Test
    fun appMethods() {
        assumeTrue("gst is not built for this machine", isLoaded)
        val info = GstJson.decodeFromJsonElement(AppInfo.serializer(), call("app.info")["result"]!!)
        assertEquals("test-phone", info.node.name)
        assertTrue(info.publicKey, info.publicKey.startsWith("ssh-ed25519 ") && info.publicKey.endsWith(" gst@test-phone"))
        val invalid = call("app.check", """{"destination":"-oProxyCommand=x"}""")["error"]!!.jsonObject
        assertEquals("invalid_params", invalid["code"]!!.jsonPrimitive.content)
        // The daemon's methods answer only while it runs.
        assertTrue(call("daemon.status")["error"].toString().contains("not running"))
    }

    @Test
    fun daemonCallsTheSystem() {
        assumeTrue("gst is not built for this machine", isLoaded)
        val asked = java.util.concurrent.LinkedBlockingQueue<InterfaceSettings>()
        Core.start(object : Core.System {
            override fun apply(settings: ByteArray): Int {
                asked += GstJson.decodeFromString(InterfaceSettings.serializer(), settings.decodeToString())
                throw IllegalStateException("no interface on this machine")
            }

            override fun forward(query: ByteArray): ByteArray? = null
        })
        try {
            val settings = asked.poll(20, java.util.concurrent.TimeUnit.SECONDS)
            assertTrue("gst asked for no interface", settings != null)
            assertEquals("198.18.0.53", settings!!.dns)
            assertEquals(32, settings.routes.last().prefixLength)
            // What Java threw is what the daemon reports.
            val deadline = System.currentTimeMillis() + 20_000
            var error = ""
            while (System.currentTimeMillis() < deadline && "no interface on this machine" !in error) {
                val status = call("daemon.status")["result"]?.let { GstJson.decodeFromJsonElement(DaemonStatus.serializer(), it) }
                error = status?.net?.error ?: ""
                Thread.sleep(100)
            }
            assertTrue(error, "no interface on this machine" in error)
        } finally {
            Core.stop()
        }
        assertTrue(call("daemon.status")["error"].toString().contains("not running"))
    }
}
