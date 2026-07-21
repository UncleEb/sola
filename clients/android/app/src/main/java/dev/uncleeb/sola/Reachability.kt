package dev.uncleeb.sola

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.net.HttpURLConnection
import java.net.URL

/**
 * Quick "can I reach the dashboard right now?" probe, used purely as a routing
 * hint for the auto-switch (direct vs. tunnel) — NOT as a hard gate. A failure
 * just means "try the tunnel," and the WebView's own offline screen is the final
 * fallback, so a short timeout is fine.
 *
 * "Reachable" is path-agnostic: on the LAN it succeeds directly; if the user
 * already runs a VPN that routes to the server, it succeeds through that; once
 * Sola's own tunnel is up, it succeeds through that. All we care about is whether
 * the dashboard answers.
 */
object Reachability {
    private const val PROBE_PATH = "/api/status"

    suspend fun isReachable(baseUrl: String, timeoutMs: Int): Boolean = withContext(Dispatchers.IO) {
        var conn: HttpURLConnection? = null
        runCatching {
            conn = (URL(baseUrl.trimEnd('/') + PROBE_PATH).openConnection() as HttpURLConnection).apply {
                connectTimeout = timeoutMs
                readTimeout = timeoutMs
                requestMethod = "GET"
                instanceFollowRedirects = true
            }
            // Any HTTP answer proves the dashboard is reachable by some path.
            conn!!.responseCode in 200..599
        }.also { conn?.disconnect() }.getOrDefault(false)
    }
}
