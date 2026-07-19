package com.alro.sola

import android.os.Bundle
import android.view.View
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.widget.doAfterTextChanged
import com.alro.sola.databinding.ActivityWireguardConfigBinding
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions

/**
 * Lets the user provide a WireGuard configuration — by pasting the `.conf` text
 * or scanning its QR code — and stores it encrypted via [WireGuardConfigStore].
 *
 * This is the capture + storage layer only. Actually bringing the tunnel up
 * (Android VpnService + the WireGuard backend) is a separate, later piece; this
 * page produces the config it will consume.
 */
class WireGuardConfigActivity : AppCompatActivity() {

    private lateinit var binding: ActivityWireguardConfigBinding

    // ZXing scanner. A QR-encoded WireGuard config is just the .conf text, so we
    // drop the scanned contents straight into the input field.
    private val scanLauncher = registerForActivityResult(ScanContract()) { result ->
        result.contents?.let { binding.inputConfig.setText(it) }
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
        updateRemoveVisibility()

        binding.inputConfig.doAfterTextChanged { updateSummary() }
        binding.buttonScan.setOnClickListener { launchScanner() }
        binding.buttonSave.setOnClickListener { onSave() }
        binding.buttonRemove.setOnClickListener { onRemove() }
    }

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
        finish()
    }

    private fun onRemove() {
        WireGuardConfigStore.clear(this)
        binding.inputConfig.setText("")
        updateSummary()
        updateRemoveVisibility()
        Toast.makeText(this, R.string.wg_removed, Toast.LENGTH_SHORT).show()
    }

    private fun updateRemoveVisibility() {
        binding.buttonRemove.visibility =
            if (WireGuardConfigStore.hasConfig(this)) View.VISIBLE else View.GONE
    }
}
