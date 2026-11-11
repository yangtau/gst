package gst.app

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.encodeToJsonElement
import kotlinx.serialization.json.jsonObject

/** An error that gst reports, in its own words. */
class GstError(val code: String, override val message: String) : Exception(message)

/**
 * Calls of gst's methods, typed. The methods and their results are those
 * of the gst command's own conversation with its daemon
 * (docs/design/mesh.md), and the "app.…" methods of internal/mobile.
 */
class Gst(
    /** Sends a request, and returns the answer. */
    @PublishedApi internal val transport: suspend (ByteArray) -> ByteArray = { request ->
        withContext(Dispatchers.IO) { Core.call(request) }
    },
) {
    @Serializable
    private class Error(val code: String = "failed", val message: String = "")

    /** Calls [method] and returns its result. */
    suspend inline fun <reified R> call(method: String, params: JsonElement = JsonObject(emptyMap())): R =
        GstJson.decodeFromJsonElement(send(method, params) ?: JsonObject(emptyMap()))

    /** Calls [method] for what it does. */
    suspend fun run(method: String, params: JsonElement = JsonObject(emptyMap())) {
        send(method, params)
    }

    @PublishedApi
    internal suspend fun send(method: String, params: JsonElement): JsonElement? {
        val request = buildJsonObject {
            put("method", GstJson.encodeToJsonElement(method))
            put("params", params)
        }
        val answer = GstJson.parseToJsonElement(transport(request.toString().encodeToByteArray()).decodeToString()).jsonObject
        answer["error"]?.takeIf { it != JsonNull }?.let {
            val error = GstJson.decodeFromJsonElement<Error>(it)
            throw GstError(error.code, error.message)
        }
        return answer["result"]?.takeIf { it != JsonNull }
    }
}

/** The params of a call. */
inline fun <reified P> params(value: P): JsonElement = GstJson.encodeToJsonElement(value)
