package com.alro.sola

import android.app.Activity
import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.net.VpnService
import android.os.Bundle
import android.provider.Settings
import android.util.Log
import android.view.View
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.widget.doAfterTextChanged
import androidx.lifecycle.lifecycleScope
import com.alro.sola.databinding.ActivityWireguardConfigBinding
import com.google.android.material.dialog.MaterialAlertDialogBuilder
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import com.wireguard.android.backend.Tunnel
import kotlinx.coroutines.launch

/**
 * Lets the user provide a WireGuard configuration — by pasting the `.conf` text
 * or scanning its QR code — store it encrypted ([WireGuardConfigStore]), and
 * bring the tunnel up or down ([TunnelController]).
 *
 * Tunnel control here is manual. The seamless "try LAN first, else tunnel"
 * auto-switch is a separate, later step layered on top of this.
 */
class WireGuardConfigActivity : AppCompatActivity() {

    private lateinit var binding: ActivityWireguardConfigBinding

    // A QR-encoded WireGuard config is just the .conf text.
    private val scanLauncher = registerForActivityResult(ScanContract()) { result ->
        result.contents?.let { binding.inputConfig.setText(it) }
    }

    // System VPN consent dialog (shown once, before the first tunnel). On approval
    // we proceed to bring the tunnel up.
    private val vpnConsentLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
            if (result.resultCode == Activity.RESULT_OK) {
                connectTunnel()
            } else {
                // Consent didn't complete — commonly because another VPN (or an
                // always-on VPN) holds the single VPN slot and the system dialog
                // never resolves. Point the user at the likely cause.
                showTunnelState(TunnelController.currentState(this))
                Toast.makeText(this, R.string.wg_consent_declined, Toast.LENGTH_LONG).show()
            }
        }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityWireguardConfigBinding.inflate(layoutInflater)
        setContentView(binding.root)

        setSupportActionBar(binding.toolbar)
        supportActionBar?.setDisplayHomeAsUpEnabled(true)
        binding.toolbar.setNavigationOnClickListener { finish() }

        // Prefill an existing config so it can be viewed or edited.
        WireGuardConfigStore.load(this)?.let { binding.inputConfig.setText(it) }
        updateSummary()
        updateControls()

        binding.inputConfig.doAfterTextChanged { updateSummary() }
        binding.buttonScan.setOnClickListener { launchScanner() }
        binding.buttonSave.setOnClickListener { onSave() }
        binding.buttonRemove.setOnClickListener { onRemove() }
        binding.buttonTunnel.setOnClickListener { onTunnelToggle() }
    }

    override fun onResume() {
        super.onResume()
        // Reflect live state changes (the backend may report them off-thread).
        TunnelController.stateListener = { state -> runOnUiThread { showTunnelState(state) } }
        showTunnelState(TunnelController.currentState(this))
    }

    override fun onPause() {
        super.onPause()
        TunnelController.stateListener = null
    }

    // --- QR / config entry ---------------------------------------------------

    private fun launchScanner() {
        val options = ScanOptions()
            .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            .setPrompt(getString(R.string.wg_scan_prompt))
            .setBeepEnabled(false)
            .setOrientationLocked(false)
        scanLauncher.launch(options)
    }

    private fun currentText(): String = binding.inputConfig.text?.toString().orEmpty().trim()

    /** Shows a non-secret confirmation of what was entered (never the keys). */
    private fun updateSummary() {
        val text = currentText()
        if (text.isEmpty()) {
            binding.summary.text = getString(R.string.wg_summary_empty)
            return
        }
        val cfg = WireGuardConfigParser.parse(text)
        val dash = getString(R.string.wg_summary_placeholder)
        binding.summary.text = buildString {
            appendLine(getString(R.string.wg_summary_endpoint, cfg.endpoint ?: dash))
            appendLine(getString(R.string.wg_summary_address, cfg.interfaceAddress ?: dash))
            append(getString(R.string.wg_summary_allowed, cfg.allowedIps ?: dash))
        }
    }

    private fun onSave() {
        val text = currentText()
        val cfg = WireGuardConfigParser.parse(text)
        if (!cfg.isValid) {
            val message = getString(R.string.wg_error_missing, cfg.missingFields.joinToString(", "))
            Toast.makeText(this, message, Toast.LENGTH_LONG).show()
            return
        }
        WireGuardConfigStore.save(this, text)
        Toast.makeText(this, R.string.wg_saved, Toast.LENGTH_SHORT).show()
        updateControls()
    }

    private fun onRemove() {
        // If the tunnel is up on the config we're deleting, take it down first.
        if (TunnelController.currentState(this) == Tunnel.State.UP) disconnectTunnel()
        WireGuardConfigStore.clear(this)
        binding.inputConfig.setText("")
        updateSummary()
        updateControls()
        Toast.makeText(this, R.string.wg_removed, Toast.LENGTH_SHORT).show()
    }

    // --- Tunnel --------------------------------------------------------------

    private fun onTunnelToggle() {
        if (TunnelController.currentState(this) == Tunnel.State.UP) {
            disconnectTunnel()
            return
        }
        // Connect from whatever's in the field (saving it), falling back to a
        // previously-saved config if the field is empty. This way the button
        // always does something — connect, or tell you what's missing.
        val config = resolveConfigOrWarn() ?: return
        WireGuardConfigStore.save(this, config)
        updateControls()

        // Android allows only one active VPN. If another one already holds the
        // slot, our consent dialog silently aborts — so catch it up front with a
        // clear explanation instead of a baffling no-op.
        if (isAnotherVpnActive()) {
            showAnotherVpnDialog()
            return
        }

        // First tunnel needs one-time VPN consent from the user.
        val consent = VpnService.prepare(this)
        if (consent != null) vpnConsentLauncher.launch(consent) else connectTunnel()
    }

    /**
     * True if some VPN is already active. We only call this while our own tunnel
     * is down (we're trying to connect), so any VPN transport belongs to another
     * app. Note: this can't see an always-on VPN that's configured but currently
     * disconnected — that case is caught by the consent-declined path instead.
     */
    private fun isAnotherVpnActive(): Boolean {
        val cm = getSystemService(ConnectivityManager::class.java) ?: return false
        return cm.allNetworks.any { network ->
            cm.getNetworkCapabilities(network)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true
        }
    }

    private fun showAnotherVpnDialog() {
        MaterialAlertDialogBuilder(this)
            .setTitle(R.string.wg_other_vpn_title)
            .setMessage(R.string.wg_other_vpn_message)
            .setPositiveButton(R.string.wg_open_vpn_settings) { _, _ ->
                runCatching { startActivity(Intent(Settings.ACTION_VPN_SETTINGS)) }
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    /** The config to connect with: the current (valid) text, else a saved one. */
    private fun resolveConfigOrWarn(): String? {
        val text = currentText()
        if (text.isNotEmpty()) {
            val cfg = WireGuardConfigParser.parse(text)
            if (cfg.isValid) return text
            Toast.makeText(
                this,
                getString(R.string.wg_error_missing, cfg.missingFields.joinToString(", ")),
                Toast.LENGTH_LONG,
            ).show()
            return null
        }
        WireGuardConfigStore.load(this)?.let { return it }
        Toast.makeText(this, R.string.wg_need_config, Toast.LENGTH_SHORT).show()
        return null
    }

    private fun connectTunnel() {
        val config = WireGuardConfigStore.load(this) ?: return
        showBusy()
        lifecycleScope.launch {
            runCatching { TunnelController.connect(this@WireGuardConfigActivity, config) }
                .onSuccess { showTunnelState(it) }
                .onFailure { error ->
                    Log.e(TAG, "Tunnel connect failed", error)
                    showTunnelState(TunnelController.currentState(this@WireGuardConfigActivity))
                    Toast.makeText(
                        this@WireGuardConfigActivity,
                        getString(R.string.wg_tunnel_error, describe(error)),
                        Toast.LENGTH_LONG,
                    ).show()
                }
        }
    }

    private fun disconnectTunnel() {
        showBusy()
        lifecycleScope.launch {
            runCatching { TunnelController.disconnect(this@WireGuardConfigActivity) }
            showTunnelState(TunnelController.currentState(this@WireGuardConfigActivity))
        }
    }

    private fun showBusy() {
        binding.buttonTunnel.isEnabled = false
        binding.tunnelStatus.text =
            getString(R.string.wg_tunnel_status, getString(R.string.wg_state_toggle))
    }

    private fun showTunnelState(state: Tunnel.State) {
        binding.tunnelStatus.text = getString(R.string.wg_tunnel_status, stateLabel(state))
        binding.buttonTunnel.text =
            getString(if (state == Tunnel.State.UP) R.string.wg_disconnect else R.string.wg_connect)
        // Always tappable — validation/feedback happens on tap, never a dead button.
        binding.buttonTunnel.isEnabled = true
    }

    /** Human-readable exception detail for the toast (class + message + cause). */
    private fun describe(error: Throwable): String = buildString {
        append(error.javaClass.simpleName)
        error.message?.let { append(": ").append(it) }
        error.cause?.let { cause ->
            append(" — cause: ").append(cause.javaClass.simpleName)
            cause.message?.let { append(": ").append(it) }
        }
    }

    private fun stateLabel(state: Tunnel.State): String = getString(
        when (state) {
            Tunnel.State.UP -> R.string.wg_state_up
            Tunnel.State.TOGGLE -> R.string.wg_state_toggle
            else -> R.string.wg_state_down
        },
    )

    private fun updateControls() {
        binding.buttonRemove.visibility =
            if (WireGuardConfigStore.hasConfig(this)) View.VISIBLE else View.GONE
    }

    companion object {
        private const val TAG = "SolaTunnel"
    }
}
