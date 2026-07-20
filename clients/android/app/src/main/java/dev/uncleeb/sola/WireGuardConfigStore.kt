package com.alro.sola

import android.content.Context

/**
 * Persists the user's WireGuard configuration, encrypted at rest via
 * [SecureStore]. We keep the raw `.conf` text (that's what the tunnel layer will
 * hand to the WireGuard backend later) rather than re-serializing parsed fields.
 */
object WireGuardConfigStore {
    private const val PREFS = "wg_config"
    private const val KEY_BLOB = "config_enc"

    private fun prefs(context: Context) =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    fun save(context: Context, rawConfig: String) {
        prefs(context).edit()
            .putString(KEY_BLOB, SecureStore.encrypt(rawConfig))
            .apply()
    }

    /** Returns the stored config, or null if none is saved / it can't be decrypted. */
    fun load(context: Context): String? {
        val blob = prefs(context).getString(KEY_BLOB, null) ?: return null
        return runCatching { SecureStore.decrypt(blob) }.getOrNull()
    }

    fun clear(context: Context) {
        prefs(context).edit().remove(KEY_BLOB).apply()
    }

    fun hasConfig(context: Context): Boolean = prefs(context).contains(KEY_BLOB)
}
