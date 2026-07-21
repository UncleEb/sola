package dev.uncleeb.sola

import android.content.Context
import com.wireguard.android.backend.Backend
import com.wireguard.android.backend.GoBackend
import com.wireguard.android.backend.Tunnel
import com.wireguard.config.Config
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.BufferedReader
import java.io.StringReader

/**
 * Owns the app's single WireGuard tunnel. Wraps the wireguard-android
 * [GoBackend], which embeds wireguard-go and drives Android's VpnService.
 *
 * A VPN is device-wide, so tunnel state is app-global and this is a singleton.
 * Backend calls block and must run off the main thread — [connect]/[disconnect]
 * hop to [Dispatchers.IO]. Callers must obtain VPN consent (VpnService.prepare)
 * before the first [connect]; the backend can't do that (it needs an Activity).
 */
object TunnelController {

    private const val TUNNEL_NAME = "sola"

    private var backend: Backend? = null
    private var tunnel: SolaTunnel? = null

    /** Notified — possibly off the main thread — whenever the tunnel state changes. */
    var stateListener: ((Tunnel.State) -> Unit)? = null

    private fun backend(context: Context): Backend =
        backend ?: GoBackend(context.applicationContext).also { backend = it }

    private fun tunnel(): SolaTunnel =
        tunnel ?: SolaTunnel().also { tunnel = it }

    fun currentState(context: Context): Tunnel.State =
        runCatching { backend(context).getState(tunnel()) }.getOrDefault(Tunnel.State.DOWN)

    /** Brings the tunnel up with the given raw wg-quick config. Returns the new state. */
    suspend fun connect(context: Context, rawConfig: String): Tunnel.State =
        withContext(Dispatchers.IO) {
            val config = Config.parse(BufferedReader(StringReader(rawConfig)))
            backend(context).setState(tunnel(), Tunnel.State.UP, config)
        }

    suspend fun disconnect(context: Context): Tunnel.State =
        withContext(Dispatchers.IO) {
            backend(context).setState(tunnel(), Tunnel.State.DOWN, null)
        }

    private class SolaTunnel : Tunnel {
        override fun getName(): String = TUNNEL_NAME
        override fun onStateChange(newState: Tunnel.State) {
            stateListener?.invoke(newState)
        }
    }
}
