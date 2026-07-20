package com.alro.sola

import android.content.Intent
import android.os.Bundle
import androidx.appcompat.app.AppCompatActivity
import com.alro.sola.databinding.ActivitySettingsBinding

/**
 * Launch screen. Collects an IP/host + port, validates the *format*, and opens
 * the dashboard. If a server was remembered on a previous run, we skip this
 * screen and go straight to the dashboard.
 *
 * We intentionally do NOT probe the server for reachability here. A pre-flight
 * probe blocks valid-but-slow connections (notably over a VPN like WireGuard,
 * where the tunnel needs a moment to wake up) and bounces the user back to this
 * screen whenever they're away from the network. Instead we just load the
 * dashboard and let [WebViewActivity] handle an unreachable server gracefully
 * (offline screen + auto-reconnect) — the same "just load it" approach apps like
 * Home Assistant and Jellyfin take.
 */
class SettingsActivity : AppCompatActivity() {

    private lateinit var binding: ActivitySettingsBinding

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val forceSettings = intent.getBooleanExtra(EXTRA_FORCE_SETTINGS, false)
        if (!forceSettings && ServerConfig.hasRememberedServer(this)) {
            openDashboard(ServerConfig.savedHost(this)!!, ServerConfig.savedPort(this))
            return
        }

        binding = ActivitySettingsBinding.inflate(layoutInflater)
        setContentView(binding.root)

        // Prefill last-used values for convenience.
        ServerConfig.savedHost(this)?.let { binding.inputHost.setText(it) }
        binding.inputPort.setText(ServerConfig.savedPort(this).toString())

        binding.buttonConnect.setOnClickListener { onConnectClicked() }
        binding.buttonRemoteAccess.setOnClickListener {
            startActivity(Intent(this, WireGuardConfigActivity::class.java))
        }
    }

    private fun onConnectClicked() {
        val host = binding.inputHost.text?.toString()?.trim().orEmpty()
        val portText = binding.inputPort.text?.toString()?.trim().orEmpty()
        val remember = binding.checkRemember.isChecked

        binding.textStatus.text = ""

        if (host.isEmpty()) {
            showError(getString(R.string.error_host_empty)); return
        }
        if (!ServerConfig.isValidHost(host)) {
            showError(getString(R.string.error_host_invalid)); return
        }
        val port = portText.toIntOrNull()
        if (port == null || !ServerConfig.isValidPort(port)) {
            showError(getString(R.string.error_port_invalid)); return
        }

        ServerConfig.save(this, host, port, remember)
        openDashboard(host, port)
    }

    private fun openDashboard(host: String, port: Int) {
        startActivity(
            Intent(this, WebViewActivity::class.java)
                .putExtra(WebViewActivity.EXTRA_URL, ServerConfig.urlFor(host, port)),
        )
        finish()
    }

    private fun showError(message: String) {
        binding.textStatus.text = message
    }

    companion object {
        const val EXTRA_FORCE_SETTINGS = "force_settings"
    }
}
