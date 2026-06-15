// WebView shell for the active backend-hosted frontend.
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
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.core.content.PermissionChecker
import androidx.core.net.toUri
import androidx.webkit.WebSettingsCompat
import androidx.webkit.WebViewFeature
import com.fghbuild.gomode.R

@SuppressLint("SetJavaScriptEnabled")
@Composable
fun WebShellScreen(
    initialURL: String,
    onOpenSettings: () -> Unit,
    onHostedPageLoaded: () -> Unit = {},
) {
    val context = LocalContext.current
    val hostURL = remember(initialURL) { goModeHostURL(initialURL) }
    var pageFailed by remember(hostURL) { mutableStateOf<String?>(null) }
    var loading by remember(hostURL) { mutableStateOf(true) }
    var fileChooserCallback by remember { mutableStateOf<ValueCallback<Array<Uri>>?>(null) }
    var loadedHostURL by remember { mutableStateOf<String?>(null) }
    val currentOnHostedPageLoaded by rememberUpdatedState(onHostedPageLoaded)
    val fileChooserLauncher = rememberLauncherForActivityResult(ActivityResultContracts.GetMultipleContents()) { uris ->
        fileChooserCallback?.onReceiveValue(uris.toTypedArray())
        fileChooserCallback = null
    }
    val webView = remember(context) {
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

                override fun onPageFinished(view: WebView, url: String?) {
                    loading = false
                    currentOnHostedPageLoaded()
                }

                override fun onReceivedError(
                    view: WebView,
                    request: WebResourceRequest,
                    error: WebResourceError,
                ) {
                    if (request.isForMainFrame) {
                        loading = false
                        pageFailed = error.description?.toString() ?: "Page load failed."
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
        AndroidView(
            factory = { webView },
            update = { view ->
                if (loadedHostURL != hostURL) {
                    loading = true
                    pageFailed = null
                    loadedHostURL = hostURL
                    view.loadUrl(hostURL)
                }
            },
            modifier = Modifier.fillMaxSize(),
        )
        if (loading) {
            CircularProgressIndicator(
                modifier = Modifier.align(Alignment.Center).testTag("gomode-web-loading"),
            )
        }
        pageFailed?.let { message ->
            WebRecoveryPanel(
                message = message,
                onRetry = {
                    pageFailed = null
                    loading = true
                    webView.reload()
                },
                onOpenSettings = onOpenSettings,
                modifier = Modifier.align(Alignment.Center),
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

private const val TAG = "GoModeWebShell"

private class GoModeHostBridge {
    @JavascriptInterface
    @Suppress("FunctionOnlyReturningConstant")
    fun shellVersion(): String = "1"
}

@Composable
private fun WebRecoveryPanel(
    message: String,
    onRetry: () -> Unit,
    onOpenSettings: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.padding(24.dp).testTag("gomode-web-recovery"),
        verticalArrangement = Arrangement.spacedBy(12.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text("Could not load service", style = MaterialTheme.typography.titleLarge)
        Text(message, style = MaterialTheme.typography.bodyMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Button(onClick = onRetry, modifier = Modifier.testTag("gomode-web-retry")) {
                Text("Retry")
            }
            Button(onClick = onOpenSettings, modifier = Modifier.testTag("gomode-open-settings")) {
                Text("Settings")
            }
        }
    }
}
