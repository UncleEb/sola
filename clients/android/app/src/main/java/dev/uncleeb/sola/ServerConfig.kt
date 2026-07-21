package dev.uncleeb.sola

import android.content.Context
import android.util.Patterns

/**
 * Persists and validates the Sola server address the user enters on the launch
 * screen. Stored in SharedPreferences so the app can skip straight to the
 * dashboard on subsequent launches.
 */
object ServerConfig {
    private const val PREFS = "sola_prefs"
    private const val KEY_HOST = "host"
    private const val KEY_PORT = "port"
    private const val KEY_REMEMBER = "remember"

    const val DEFAULT_PORT = 8088

    private fun prefs(context: Context) =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    fun save(context: Context, host: String, port: Int, remember: Boolean) {
        prefs(context).edit()
            .putString(KEY_HOST, host)
            .putInt(KEY_PORT, port)
            .putBoolean(KEY_REMEMBER, remember)
            .apply()
    }

    fun clear(context: Context) {
        prefs(context).edit().clear().apply()
    }

    fun savedHost(context: Context): String? = prefs(context).getString(KEY_HOST, null)

    fun savedPort(context: Context): Int = prefs(context).getInt(KEY_PORT, DEFAULT_PORT)

    /** True when a server was saved and the user asked to remember it. */
    fun hasRememberedServer(context: Context): Boolean =
        prefs(context).getBoolean(KEY_REMEMBER, false) && savedHost(context) != null

    /** Builds the dashboard base URL, e.g. http://192.168.1.50:8088 */
    fun urlFor(host: String, port: Int): String = "http://$host:$port"

    /**
     * A host is valid if it's a well-formed IPv4/IPv6 literal or a plausible
     * hostname. We keep this permissive — this is only a cheap format check to
     * catch obvious typos; the real proof is whether the dashboard loads in
     * [WebViewActivity], which shows an offline screen if it can't be reached.
     */
    fun isValidHost(host: String): Boolean {
        val h = host.trim()
        if (h.isEmpty()) return false
        if (h.contains("/") || h.contains(" ")) return false
        return Patterns.IP_ADDRESS.matcher(h).matches() ||
            Patterns.DOMAIN_NAME.matcher(h).matches() ||
            h == "localhost" ||
            h.contains(":") // crude IPv6 acceptance; probe confirms
    }

    fun isValidPort(port: Int): Boolean = port in 1..65535
}
