package com.alro.sola

import android.annotation.SuppressLint
import android.content.Intent
import android.graphics.Bitmap
import android.net.ConnectivityManager
import android.net.Network
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.TypedValue
import android.view.GestureDetector
import android.view.Menu
import android.view.MenuItem
import android.view.MotionEvent
import android.view.View
import android.webkit.RenderProcessGoneDetail
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.OnBackPressedCallback
import androidx.appcompat.app.AppCompatActivity
import com.alro.sola.databinding.ActivityWebviewBinding
import kotlin.math.abs

/**
 * Hosts the Sola dashboard in a full-screen WebView. Responsibilities beyond
 * "load the URL": enable the JS/DOM features the dashboard needs, wire the
 * hardware back button to WebView history, and offer a way back to the launch
 * screen to change servers.
 *
 * The toolbar is hidden by default so the dashboard fills the screen. A
 * deliberate swipe down from the top edge slides it in; it then auto-hides after
 * a few idle seconds.
 */
class WebViewActivity : AppCompatActivity() {

    private lateinit var binding: ActivityWebviewBinding

    private var toolbarShown = false
    private var toolbarHeightPx = 0
    private val autoHideHandler = Handler(Looper.getMainLooper())
    private val autoHideRunnable = Runnable { hideToolbar() }
    private lateinit var revealDetector: GestureDetector

    private var dashboardUrl = ""
    private var loadFailed = false
    private var rendererGone = false
    private var reloadOnResume = false
    private var networkCallbackRegistered = false
    private val connectivityManager by lazy { getSystemService(ConnectivityManager::class.java) }

    // When the network (or WireGuard tunnel) comes back, retry a failed load.
    private val networkCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            runOnUiThread { if (loadFailed) retryLoad() }
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityWebviewBinding.inflate(layoutInflater)
        setContentView(binding.root)

        val url = intent.getStringExtra(EXTRA_URL)
        if (url.isNullOrBlank()) {
            finish()
            return
        }
        dashboardUrl = url

        setSupportActionBar(binding.toolbar)
        toolbarHeightPx = resolveActionBarSize()
        setupRevealGesture()
        configureWebView()
        wireBackButton()

        binding.buttonRetry.setOnClickListener { retryLoad() }
        binding.buttonOfflineChangeServer.setOnClickListener { changeServer() }

        // Always load fresh. We deliberately do NOT persist WebView state across
        // recreation: WebView.saveState() can serialize a huge page/history blob
        // that overflows the Binder transaction limit and crashes the app with
        // TransactionTooLargeException. The dashboard is live data, so a fresh
        // load on recreation is both safe and more correct. (Rotation doesn't
        // recreate this activity — see android:configChanges in the manifest.)
        binding.webView.loadUrl(dashboardUrl)
    }

    override fun onStart() {
        super.onStart()
        if (!networkCallbackRegistered) {
            runCatching { connectivityManager?.registerDefaultNetworkCallback(networkCallback) }
                .onSuccess { networkCallbackRegistered = true }
        }
    }

    override fun onStop() {
        super.onStop()
        if (networkCallbackRegistered) {
            runCatching { connectivityManager?.unregisterNetworkCallback(networkCallback) }
            networkCallbackRegistered = false
        }
    }

    override fun onResume() {
        super.onResume()
        // Refresh on returning to the foreground so a stale page (e.g. the
        // network dropped while we were backgrounded) reloads and, if the server
        // is now unreachable, surfaces the offline screen instead of pretending
        // to still be connected. Skipped on the initial resume (onCreate loads).
        if (reloadOnResume && !rendererGone) {
            binding.webView.loadUrl(dashboardUrl)
        }
        reloadOnResume = true
    }

    // --- Auto-hiding toolbar --------------------------------------------------

    private fun setupRevealGesture() {
        val edgePx = dp(140)   // swipe must start within this distance of the top
        val revealPx = dp(40)  // ...and travel at least this far downward
        revealDetector = GestureDetector(this, object : GestureDetector.SimpleOnGestureListener() {
            override fun onScroll(
                e1: MotionEvent?,
                e2: MotionEvent,
                distanceX: Float,
                distanceY: Float,
            ): Boolean {
                if (e1 != null && !toolbarShown) {
                    val dy = e2.y - e1.y
                    val dx = e2.x - e1.x
                    // Downward drag that begins near the top edge and is mostly
                    // vertical — a deliberate "pull the header down" gesture.
                    if (e1.y <= edgePx && dy > revealPx && abs(dy) > abs(dx)) {
                        showToolbar()
                    }
                }
                return false // never consume — the WebView still scrolls normally
            }
        })
    }

    // Feed every touch to the reveal detector without stealing it from the WebView.
    override fun dispatchTouchEvent(ev: MotionEvent): Boolean {
        revealDetector.onTouchEvent(ev)
        return super.dispatchTouchEvent(ev)
    }

    private fun showToolbar() {
        autoHideHandler.removeCallbacks(autoHideRunnable)
        if (!toolbarShown) {
            toolbarShown = true
            binding.toolbar.visibility = View.VISIBLE
            binding.toolbar.translationY = -toolbarHeightPx.toFloat()
            binding.toolbar.animate().translationY(0f).setDuration(180).start()
        }
        autoHideHandler.postDelayed(autoHideRunnable, AUTO_HIDE_MS)
    }

    private fun hideToolbar() {
        if (!toolbarShown) return
        toolbarShown = false
        binding.toolbar.animate()
            .translationY(-toolbarHeightPx.toFloat())
            .setDuration(180)
            .withEndAction { binding.toolbar.visibility = View.GONE }
            .start()
    }

    // Keep the toolbar up while its overflow menu is open, then resume the timer.
    override fun onMenuOpened(featureId: Int, menu: Menu): Boolean {
        autoHideHandler.removeCallbacks(autoHideRunnable)
        return super.onMenuOpened(featureId, menu)
    }

    override fun onPanelClosed(featureId: Int, menu: Menu) {
        super.onPanelClosed(featureId, menu)
        if (toolbarShown) autoHideHandler.postDelayed(autoHideRunnable, AUTO_HIDE_MS)
    }

    override fun onDestroy() {
        autoHideHandler.removeCallbacks(autoHideRunnable)
        super.onDestroy()
    }

    private fun resolveActionBarSize(): Int {
        val tv = TypedValue()
        return if (theme.resolveAttribute(android.R.attr.actionBarSize, tv, true)) {
            TypedValue.complexToDimensionPixelSize(tv.data, resources.displayMetrics)
        } else {
            dp(56)
        }
    }

    private fun dp(value: Int): Int = (value * resources.displayMetrics.density).toInt()

    private fun configureWebView() = with(binding.webView.settings) {
        // The dashboard is a live JS app (status polling, flow animation), so
        // JavaScript and DOM storage are both required.
        javaScriptEnabled = true
        domStorageEnabled = true
        loadWithOverviewMode = true
        useWideViewPort = true
        builtInZoomControls = true
        displayZoomControls = false
        mediaPlaybackRequiresUserGesture = false

        binding.webView.webViewClient = object : WebViewClient() {
            // Keep all navigation inside the WebView (don't kick out to a browser).
            override fun shouldOverrideUrlLoading(
                view: WebView?,
                request: WebResourceRequest?,
            ): Boolean = false

            override fun onPageStarted(view: WebView?, url: String?, favicon: Bitmap?) {
                // New attempt: assume success unless onReceivedError says otherwise.
                loadFailed = false
            }

            override fun onReceivedError(
                view: WebView?,
                request: WebResourceRequest?,
                error: WebResourceError?,
            ) {
                // Only a failure of the main document means "can't reach the
                // dashboard" — ignore failures of individual sub-resources.
                if (request?.isForMainFrame == true) {
                    loadFailed = true
                    showOffline()
                }
            }

            override fun onPageFinished(view: WebView?, url: String?) {
                if (!loadFailed) showContent()
            }

            override fun onRenderProcessGone(
                view: WebView?,
                detail: RenderProcessGoneDetail?,
            ): Boolean {
                // The WebView's render process died (usually the system reclaiming
                // memory). If we don't return true here, the framework kills the
                // whole app. We can't reuse a WebView whose renderer is gone, so
                // we show the offline screen; Retry rebuilds the activity cleanly.
                rendererGone = true
                loadFailed = true
                showOffline()
                return true
            }
        }

        binding.webView.webChromeClient = object : WebChromeClient() {
            override fun onProgressChanged(view: WebView?, newProgress: Int) {
                binding.webProgress.progress = newProgress
                binding.webProgress.visibility =
                    if (newProgress in 1..99) View.VISIBLE else View.GONE
            }
        }
    }

    // --- Offline handling & reconnect ----------------------------------------

    private fun showOffline() {
        binding.offlineUrl.text = dashboardUrl
        binding.buttonRetry.isEnabled = true
        binding.buttonRetry.text = getString(R.string.offline_retry)
        binding.webProgress.visibility = View.GONE
        binding.offlineView.visibility = View.VISIBLE
    }

    private fun showContent() {
        binding.offlineView.visibility = View.GONE
    }

    private fun retryLoad() {
        // A WebView whose renderer died can't be reused — rebuild the activity.
        if (rendererGone) {
            recreate()
            return
        }
        loadFailed = false
        binding.buttonRetry.isEnabled = false
        binding.buttonRetry.text = getString(R.string.offline_reconnecting)
        binding.webView.loadUrl(dashboardUrl)
    }

    private fun changeServer() {
        // Forget the remembered server and return to the launch screen.
        ServerConfig.clear(this)
        startActivity(
            Intent(this, SettingsActivity::class.java)
                .putExtra(SettingsActivity.EXTRA_FORCE_SETTINGS, true)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP),
        )
        finish()
    }

    private fun wireBackButton() {
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                if (binding.webView.canGoBack()) {
                    binding.webView.goBack()
                } else {
                    isEnabled = false
                    onBackPressedDispatcher.onBackPressed()
                }
            }
        })
    }

    override fun onCreateOptionsMenu(menu: Menu): Boolean {
        menu.add(0, MENU_RELOAD, 0, R.string.menu_reload)
            .setShowAsAction(MenuItem.SHOW_AS_ACTION_NEVER)
        menu.add(0, MENU_REMOTE_ACCESS, 1, R.string.menu_remote_access)
            .setShowAsAction(MenuItem.SHOW_AS_ACTION_NEVER)
        menu.add(0, MENU_CHANGE_SERVER, 2, R.string.menu_change_server)
            .setShowAsAction(MenuItem.SHOW_AS_ACTION_NEVER)
        return true
    }

    override fun onOptionsItemSelected(item: MenuItem): Boolean = when (item.itemId) {
        MENU_RELOAD -> {
            binding.webView.reload(); true
        }
        MENU_REMOTE_ACCESS -> {
            startActivity(Intent(this, WireGuardConfigActivity::class.java)); true
        }
        MENU_CHANGE_SERVER -> {
            changeServer(); true
        }
        else -> super.onOptionsItemSelected(item)
    }

    companion object {
        const val EXTRA_URL = "url"
        private const val MENU_RELOAD = 1
        private const val MENU_REMOTE_ACCESS = 2
        private const val MENU_CHANGE_SERVER = 3
        private const val AUTO_HIDE_MS = 3500L
    }
}
