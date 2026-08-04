package pro.zackpollard.fujicull

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder

/** One catalog entry, as served by /api/state. */
data class Shot(val id: String, val folder: String, val base: String, val kind: String, val date: String = "")

/**
 * Thin client for the engine's HTTP API. Local (loopback) or remote (a camera
 * host on the LAN). For a remote host `key` authenticates: JSON calls send it as
 * the x-api-key header, media URLs carry it as ?key= so Coil / the video player
 * authenticate without headers.
 */
@androidx.compose.runtime.Stable
class Api(private val base: String, private val key: String = "") {
    /** Loopback convenience for the embedded engine. */
    constructor(port: Long) : this("http://127.0.0.1:$port")

    private fun keyed(url: String): String =
        if (key.isEmpty()) url
        else url + (if (url.contains('?')) "&" else "?") + "key=" + URLEncoder.encode(key, "UTF-8")

    fun thumbUrl(id: String, orient: Char = '0', tick: Int = 0): String {
        var u = "$base/api/thumb?id=" + URLEncoder.encode(id, "UTF-8")
        if (orient > '1') u += "&o=$orient"
        // resume counter: busts Coil's per-URL cache so cells that failed
        // while the process was frozen retry after foregrounding
        if (tick > 0) u += "&rt=$tick"
        return keyed(u)
    }

    fun imageUrl(id: String): String =
        keyed("$base/api/image?id=" + URLEncoder.encode(id, "UTF-8"))

    fun videoUrl(id: String): String =
        keyed("$base/api/video?id=" + URLEncoder.encode(id, "UTF-8"))

    fun videoHeadUrl(id: String): String =
        keyed("$base/api/videohead?id=" + URLEncoder.encode(id, "UTF-8"))

    /** Remote readiness probe (open endpoint); false if unreachable. */
    suspend fun health(): JSONObject? = withContext(Dispatchers.IO) {
        runCatching { JSONObject(get("/api/health")) }.getOrNull()
    }

    suspend fun state(): Pair<List<Shot>, MutableMap<String, String>> = withContext(Dispatchers.IO) {
        val o = JSONObject(get("/api/state"))
        val shots = mutableListOf<Shot>()
        val arr = o.getJSONArray("shots")
        for (i in 0 until arr.length()) {
            val s = arr.getJSONObject(i)
            shots.add(
                Shot(
                    s.getString("id"), s.getString("folder"), s.getString("base"),
                    s.getString("kind"), s.optString("date"),
                ),
            )
        }
        val decisions = mutableMapOf<String, String>()
        val d = o.getJSONObject("decisions")
        for (k in d.keys()) decisions[k] = d.getString(k)
        shots to decisions
    }

    suspend fun thumbStates(): Triple<String, String, String> = withContext(Dispatchers.IO) {
        val o = JSONObject(get("/api/thumbs"))
        Triple(o.optString("states"), o.optString("orient"), o.optString("immich"))
    }

    suspend fun decide(id: String, decision: String) = withContext(Dispatchers.IO) {
        post("/api/decision", JSONObject().put("id", id).put("decision", decision.ifEmpty { "clear" }))
    }

    suspend fun startImport(dest: String, album: String, reimport: Boolean = false) = withContext(Dispatchers.IO) {
        // reimport re-sends shots already imported; off by default so a
        // finished event is not pushed into the next event's album.
        post("/api/import", JSONObject().put("dest", dest).put("album", album)
            .put("reimport", reimport))
    }

    suspend fun status(): JSONObject = withContext(Dispatchers.IO) {
        JSONObject(get("/api/status"))
    }

    /** Sweep origin for thumbnail work — call as the grid viewport moves. */
    suspend fun thumbHint(index: Int) = withContext(Dispatchers.IO) {
        runCatching { post("/api/thumbhint", JSONObject().put("index", index)) }
    }

    /** Buffer-window center — call as the viewer page changes. */
    suspend fun cursor(index: Int) = withContext(Dispatchers.IO) {
        runCatching { post("/api/cursor", JSONObject().put("index", index)) }
    }

    /** Clears a failed fetch so the engine tries the shot again now. */
    suspend fun retryShot(id: String) = withContext(Dispatchers.IO) {
        runCatching { post("/api/retry", JSONObject().put("id", id)) }
    }

    /** Hands the camera back after a one-shot stream use (poster grab). */
    suspend fun releaseStream() = withContext(Dispatchers.IO) {
        runCatching { post("/api/releasestream", JSONObject()) }
    }

    /** Adds an app-side event to the engine log (diagnostics screen). */
    suspend fun logEvent(msg: String) = withContext(Dispatchers.IO) {
        runCatching { post("/api/log", JSONObject().put("msg", msg)) }
    }

    /** Drops the catalog cache; takes effect on the next engine start. */
    suspend fun rescan() = withContext(Dispatchers.IO) {
        runCatching { post("/api/rescan", JSONObject()) }
    }

    private fun get(path: String): String {
        val c = URL(base + path).openConnection() as HttpURLConnection
        c.connectTimeout = 5000
        c.readTimeout = 30000
        if (key.isNotEmpty()) c.setRequestProperty("x-api-key", key)
        return c.inputStream.bufferedReader().readText()
    }

    private fun post(path: String, body: JSONObject): String {
        val c = URL(base + path).openConnection() as HttpURLConnection
        c.requestMethod = "POST"
        c.doOutput = true
        c.setRequestProperty("Content-Type", "application/json")
        if (key.isNotEmpty()) c.setRequestProperty("x-api-key", key)
        c.outputStream.write(body.toString().toByteArray())
        return c.inputStream.bufferedReader().readText()
    }
}
