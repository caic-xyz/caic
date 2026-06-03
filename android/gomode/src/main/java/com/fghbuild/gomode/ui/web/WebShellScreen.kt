// WebView shell that loads the active backend-hosted frontend.
package com.fghbuild.gomode.ui.web

import android.Manifest
import android.annotation.SuppressLint
import android.net.Uri
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
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.core.content.PermissionChecker

@SuppressLint("SetJavaScriptEnabled")
@Composable
fun WebShellScreen(
    initialURL: String,
    onOpenSettings: () -> Unit,
) {
    val context = LocalContext.current
    var webView by remember { mutableStateOf<WebView?>(null) }
    var pageFailed by remember(initialURL) { mutableStateOf<String?>(null) }
    var loading by remember(initialURL) { mutableStateOf(true) }
    var canGoBack by remember { mutableStateOf(false) }
    var fileChooserCallback by remember { mutableStateOf<ValueCallback<Array<Uri>>?>(null) }
    val fileChooserLauncher = rememberLauncherForActivityResult(ActivityResultContracts.GetMultipleContents()) { uris ->
        fileChooserCallback?.onReceiveValue(uris.toTypedArray())
        fileChooserCallback = null
    }

    BackHandler(enabled = canGoBack) {
        webView?.goBack()
        canGoBack = webView?.canGoBack() == true
    }

    DisposableEffect(Unit) {
        onDispose {
            webView?.destroy()
            webView = null
        }
    }

    Box(Modifier.fillMaxSize().testTag("gomode-web-shell")) {
        AndroidView(
            factory = { factoryContext ->
                WebView(factoryContext).apply {
                    settings.javaScriptEnabled = true
                    settings.domStorageEnabled = true
                    settings.mediaPlaybackRequiresUserGesture = false
                    webViewClient = object : WebViewClient() {
                        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                            view.loadUrl(request.url.toString())
                            return true
                        }

                        override fun onPageFinished(view: WebView, url: String?) {
                            loading = false
                            canGoBack = view.canGoBack()
                        }

                        override fun onReceivedError(
                            view: WebView,
                            request: WebResourceRequest,
                            error: WebResourceError,
                        ) {
                            if (request.isForMainFrame) {
                                loading = false
                                canGoBack = view.canGoBack()
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
                    }
                    webView = this
                    loadUrl(initialURL)
                }
            },
            update = { view ->
                if (view.url != initialURL && view.originalUrl != initialURL) {
                    loading = true
                    pageFailed = null
                    view.loadUrl(initialURL)
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
                    webView?.reload()
                },
                onOpenSettings = onOpenSettings,
                modifier = Modifier.align(Alignment.Center),
            )
        }
    }
}

private fun hasPermission(context: android.content.Context, permission: String): Boolean =
    ContextCompat.checkSelfPermission(context, permission) == PermissionChecker.PERMISSION_GRANTED

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
