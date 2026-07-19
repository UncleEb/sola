package com.alro.sola

/**
 * A lightweight, non-secret summary of a WireGuard `.conf`. The raw text is what
 * we actually persist and (later) hand to the tunnel backend; this exists to
 * validate the config and show the user a confirmation of what they entered
 * without ever surfacing the private or preshared keys.
 */
data class WireGuardConfig(
    val raw: String,
    val interfaceAddress: String?,
    val dns: String?,
    val peerPublicKey: String?,
    val endpoint: String?,
    val allowedIps: String?,
    val hasPrivateKey: Boolean,
    val hasPresharedKey: Boolean,
) {
    /**
     * Required fields that are absent, by their `.conf` key name. A config is
     * valid for bringing up a tunnel when this is empty. Reporting the specific
     * missing field (rather than a generic "incomplete") is what makes the error
     * actionable — e.g. a config with everything but an Endpoint.
     */
    val missingFields: List<String>
        get() = buildList {
            if (!hasPrivateKey) add("PrivateKey")
            if (interfaceAddress.isNullOrBlank()) add("Address")
            if (peerPublicKey.isNullOrBlank()) add("PublicKey (peer)")
            if (endpoint.isNullOrBlank()) add("Endpoint")
        }

    /** The minimum needed to actually bring up a tunnel to the server. */
    val isValid: Boolean get() = missingFields.isEmpty()
}

/**
 * Minimal parser for the wg-quick INI format. It's deliberately lenient (keys
 * are matched case-insensitively, comments and blank lines are ignored) — the
 * authoritative parse will be done by the WireGuard backend when the tunnel
 * feature lands; this is just enough to validate and summarize. Only the first
 * `[Peer]` is summarized, but the full raw text is always preserved.
 */
object WireGuardConfigParser {
    fun parse(text: String): WireGuardConfig {
        var section = ""
        var address: String? = null
        var dns: String? = null
        var hasPrivateKey = false
        var peerPublicKey: String? = null
        var endpoint: String? = null
        var allowedIps: String? = null
        var hasPresharedKey = false

        text.lineSequence().forEach { rawLine ->
            val line = rawLine.substringBefore('#').substringBefore(';').trim()
            if (line.isEmpty()) return@forEach
            if (line.startsWith("[") && line.endsWith("]")) {
                section = line.substring(1, line.length - 1).trim().lowercase()
                return@forEach
            }
            val eq = line.indexOf('=')
            if (eq < 0) return@forEach
            val key = line.substring(0, eq).trim().lowercase()
            val value = line.substring(eq + 1).trim()
            when (section) {
                "interface" -> when (key) {
                    "address" -> address = value
                    "dns" -> dns = value
                    "privatekey" -> hasPrivateKey = value.isNotEmpty()
                }
                "peer" -> when (key) {
                    "publickey" -> if (peerPublicKey == null) peerPublicKey = value
                    "endpoint" -> if (endpoint == null) endpoint = value
                    "allowedips" -> if (allowedIps == null) allowedIps = value
                    "presharedkey" -> if (value.isNotEmpty()) hasPresharedKey = true
                }
            }
        }

        return WireGuardConfig(
            raw = text,
            interfaceAddress = address,
            dns = dns,
            peerPublicKey = peerPublicKey,
            endpoint = endpoint,
            allowedIps = allowedIps,
            hasPrivateKey = hasPrivateKey,
            hasPresharedKey = hasPresharedKey,
        )
    }
}
