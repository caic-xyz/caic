// WebView shell that reports active backend-hosted frontend load state.
package com.fghbuild.gomode.ui.web

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.net.Uri
import android.os.Message
import android.util.Log
import android.webkit.JavascriptInterface
import android.webkit.PermissionRequest
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.core.content.PermissionChecker
import androidx.core.net.toUri
import androidx.webkit.WebSettingsCompat
import androidx.webkit.WebViewFeature
import com.fghbuild.gomode.R
import kotlinx.coroutines.delay

@SuppressLint("SetJavaScriptEnabled")
@Composable
internal fun WebShellScreen(
    initialURL: String,
    reloadToken: Int,
    onLoadStateChanged: (WebShellLoadState) -> Unit,
    onHostedPageLoaded: () -> Unit = {},
) {
    val context = LocalContext.current
    val hostURL = remember(initialURL) { goModeHostURL(initialURL) }
    var loading by remember(hostURL) { mutableStateOf(true) }
    var loadFailed by remember(hostURL) { mutableStateOf(false) }
    var automaticRetryState by remember(hostURL) { mutableStateOf(AutomaticTimeoutRetryState()) }
    var recoveryLoadInProgress by remember(hostURL) { mutableStateOf(false) }
    var appliedReloadToken by remember(hostURL) { mutableStateOf(reloadToken) }
    var fileChooserCallback by remember(hostURL) { mutableStateOf<ValueCallback<Array<Uri>>?>(null) }
    var loadedHostURL by remember(hostURL) { mutableStateOf<String?>(null) }
    val currentHostURL by rememberUpdatedState(hostURL)
    val currentOnLoadStateChanged by rememberUpdatedState(onLoadStateChanged)
    val currentOnHostedPageLoaded by rememberUpdatedState(onHostedPageLoaded)
    val fileChooserLauncher = rememberLauncherForActivityResult(ActivityResultContracts.GetMultipleContents()) { uris ->
        fileChooserCallback?.onReceiveValue(uris.toTypedArray())
        fileChooserCallback = null
    }
    val webView = remember(context, hostURL) {
        WebView(context).apply {
            id = R.id.web_shell
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.mediaPlaybackRequiresUserGesture = false
            settings.setSupportMultipleWindows(true)
            enableWebAuthentication()
            addJavascriptInterface(GoModeHostBridge(), "goModeHost")
            webViewClient = object : WebViewClient() {
                override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                    view.loadUrl(request.url.toString())
                    return true
                }

                override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
                    if (currentHostURL != hostURL) return

                    automaticRetryState = automaticTimeoutRetryStateOnPageStarted(automaticRetryState)
                    loadFailed = false
                    loading = true
                    currentOnLoadStateChanged(
                        if (recoveryLoadInProgress) WebShellLoadState.Reconnecting else WebShellLoadState.Loading,
                    )
                }

                override fun onPageFinished(view: WebView, url: String?) {
                    if (currentHostURL != hostURL) return

                    if (!loadFailed) {
                        automaticRetryState = automaticRetryState.copy(attempts = 0)
                        recoveryLoadInProgress = false
                        currentOnLoadStateChanged(WebShellLoadState.Ready)
                    }
                    loading = automaticRetryState.pending
                    currentOnHostedPageLoaded()
                }

                override fun onReceivedError(
                    view: WebView,
                    request: WebResourceRequest,
                    error: WebResourceError,
                ) {
                    if (!request.isForMainFrame || currentHostURL != hostURL) return

                    loadFailed = true
                    if (shouldAutomaticallyRetryWebLoadError(error.errorCode, automaticRetryState.attempts)) {
                        automaticRetryState = automaticRetryState.copy(
                            attempts = automaticRetryState.attempts + 1,
                            pending = true,
                        )
                        recoveryLoadInProgress = true
                        loading = true
                        currentOnLoadStateChanged(WebShellLoadState.Reconnecting)
                    } else {
                        val message = webLoadErrorMessage(error.errorCode)
                        recoveryLoadInProgress = false
                        loading = false
                        currentOnLoadStateChanged(WebShellLoadState.Failed(message))
                    }
                }
            }
            webChromeClient = object : WebChromeClient() {
                override fun onPermissionRequest(request: PermissionRequest) {
                    val grantedResources = request.resources.filter { resource ->
                        when (resource) {
                            PermissionRequest.RESOURCE_AUDIO_CAPTURE ->
                                hasPermission(context, Manifest.permission.RECORD_AUDIO)
                            PermissionRequest.RESOURCE_VIDEO_CAPTURE ->
                                hasPermission(context, Manifest.permission.CAMERA)
                            else -> true
                        }
                    }.toTypedArray()
                    if (grantedResources.size != request.resources.size) {
                        request.deny()
                        return
                    }
                    request.grant(grantedResources)
                }

                override fun onShowFileChooser(
                    webView: WebView,
                    filePathCallback: ValueCallback<Array<Uri>>,
                    fileChooserParams: FileChooserParams,
                ): Boolean {
                    fileChooserCallback?.onReceiveValue(emptyArray())
                    fileChooserCallback = filePathCallback
                    val mimeTypes = fileChooserParams.acceptTypes.filter { it.isNotBlank() }
                    fileChooserLauncher.launch(mimeTypes.firstOrNull() ?: "*/*")
                    return true
                }

                override fun onCreateWindow(
                    view: WebView,
                    isDialog: Boolean,
                    isUserGesture: Boolean,
                    resultMsg: Message,
                ): Boolean = openNewWindowInExternalBrowser(view, resultMsg) { uri ->
                    openExternalBrowser(context, uri)
                }
            }
        }
    }

    LaunchedEffect(hostURL, reloadToken) {
        if (reloadToken == appliedReloadToken) return@LaunchedEffect
        appliedReloadToken = reloadToken
        automaticRetryState = AutomaticTimeoutRetryState()
        recoveryLoadInProgress = true
        loadFailed = false
        loading = true
        currentOnLoadStateChanged(WebShellLoadState.Reconnecting)
        webView.reload()
    }

    LaunchedEffect(hostURL, automaticRetryState.pending) {
        if (!automaticRetryState.pending) return@LaunchedEffect
        delay(AUTOMATIC_TIMEOUT_RETRY_DELAY_MILLIS)
        if (automaticRetryState.pending) {
            automaticRetryState = automaticRetryState.copy(pending = false, inProgress = true)
            webView.reload()
        }
    }

    // The hosted frontend owns in-page routes, so Android Back must ask the SPA
    // router to move before falling back to WebView's document-level history.
    BackHandler {
        webView.evaluateJavascript(
            """
            const before = window.location.pathname;
            if (before !== "/") {
              window.history.back();
              window.setTimeout(() => {
                if (window.location.pathname === before) {
                  // Some SPA transitions do not leave a WebView history entry.
                  // Force the root route and notify routers listening for popstate.
                  window.history.pushState(null, "", "/");
                  window.dispatchEvent(new PopStateEvent("popstate"));
                }
              }, 100);
              true;
            } else {
              false;
            }
            """.trimIndent(),
        ) { handled ->
            // If the SPA was already at its root, use normal WebView back
            // navigation for any full-page loads that may be in history.
            if (handled != "true" && webView.canGoBack()) {
                webView.goBack()
            }
        }
    }

    DisposableEffect(webView) {
        onDispose {
            webView.destroy()
        }
    }

    Box(Modifier.fillMaxSize().statusBarsPadding().testTag("gomode-web-shell")) {
        key(hostURL) {
            AndroidView(
                factory = { webView },
                update = { view ->
                    if (loadedHostURL != hostURL) {
                        automaticRetryState = AutomaticTimeoutRetryState()
                        recoveryLoadInProgress = false
                        loadFailed = false
                        loading = true
                        currentOnLoadStateChanged(WebShellLoadState.Loading)
                        loadedHostURL = hostURL
                        view.loadUrl(hostURL)
                    }
                },
                modifier = Modifier.fillMaxSize(),
            )
        }
        if (loading) {
            CircularProgressIndicator(
                modifier = Modifier.align(Alignment.Center).testTag("gomode-web-loading"),
            )
        }
    }
}

private fun WebView.enableWebAuthentication() {
    if (WebViewFeature.isFeatureSupported(WebViewFeature.WEB_AUTHENTICATION)) {
        WebSettingsCompat.setWebAuthenticationSupport(
            settings,
            WebSettingsCompat.WEB_AUTHENTICATION_SUPPORT_FOR_APP,
        )
    } else {
        Log.w(TAG, "Installed Android WebView does not support WebAuthn.")
    }
}

private fun hasPermission(context: Context, permission: String): Boolean =
    ContextCompat.checkSelfPermission(context, permission) == PermissionChecker.PERMISSION_GRANTED

internal sealed interface WebShellLoadState {
    data object Loading : WebShellLoadState
    data object Reconnecting : WebShellLoadState
    data object Ready : WebShellLoadState
    data class Failed(val message: String) : WebShellLoadState
}

internal data class AutomaticTimeoutRetryState(
    val attempts: Int = 0,
    val pending: Boolean = false,
    val inProgress: Boolean = false,
)

internal fun automaticTimeoutRetryStateOnPageStarted(
    state: AutomaticTimeoutRetryState,
): AutomaticTimeoutRetryState = if (state.inProgress) {
    state.copy(inProgress = false)
} else {
    AutomaticTimeoutRetryState()
}

internal fun shouldAutomaticallyRetryWebLoadError(errorCode: Int, retryAttempts: Int): Boolean =
    errorCode == WebViewClient.ERROR_TIMEOUT && retryAttempts == 0

internal fun webLoadErrorMessage(errorCode: Int): String =
    if (errorCode == WebViewClient.ERROR_TIMEOUT) {
        "The service took too long to respond. Check your network connection, then retry."
    } else {
        "Could not connect to the service. Check your network connection and service address, then retry."
    }

internal fun openNewWindowInExternalBrowser(
    parentView: WebView,
    resultMsg: Message,
    openExternalUri: (Uri) -> Boolean,
): Boolean {
    val hitTestResult = parentView.hitTestResult
    newWindowRequestUriOrNull(hitTestResult.type, hitTestResult.extra)?.let { uri ->
        openExternalUri(uri)
        return false
    }

    val transport = resultMsg.obj as? WebView.WebViewTransport
    if (transport == null) {
        Log.w(TAG, "New window request missing WebView transport.")
        return false
    }

    val popupView = WebView(parentView.context).apply {
        webViewClient = object : WebViewClient() {
            private var openedExternalWindow = false

            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                openOnce(view, request.url)
                return true
            }

            override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
                url?.toUri()?.let { openOnce(view, it) }
            }

            private fun openOnce(view: WebView, uri: Uri) {
                if (openedExternalWindow) return
                openedExternalWindow = true
                openExternalUri(uri)
                view.destroy()
            }
        }
    }

    transport.webView = popupView
    resultMsg.sendToTarget()
    return true
}

internal fun newWindowRequestUriOrNull(hitTestType: Int, extra: String?): Uri? {
    if (hitTestType != WebView.HitTestResult.SRC_ANCHOR_TYPE &&
        hitTestType != WebView.HitTestResult.SRC_IMAGE_ANCHOR_TYPE
    ) {
        return null
    }
    if (extra.isNullOrBlank()) return null

    return extra.toUri().takeIf { !it.scheme.isNullOrBlank() }
}

internal fun openExternalBrowser(context: Context, uri: Uri): Boolean {
    val intent = Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE)
    if (context !is Activity) {
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
    }

    return try {
        context.startActivity(intent)
        true
    } catch (error: ActivityNotFoundException) {
        Log.w(TAG, "No browser can open URL: $uri", error)
        false
    }
}

private fun goModeHostURL(url: String): String =
    url.toUri()
        .buildUpon()
        .appendQueryParameter("goModeHost", "1")
        .build()
        .toString()

private const val AUTOMATIC_TIMEOUT_RETRY_DELAY_MILLIS = 1_000L
private const val TAG = "GoModeWebShell"

private class GoModeHostBridge {
    @JavascriptInterface
    @Suppress("FunctionOnlyReturningConstant")
    fun shellVersion(): String = "1"
}
