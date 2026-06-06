// Sandboxed WebView widget card for agent-generated HTML widgets.
package com.fghbuild.caic.ui.taskdetail

import android.annotation.SuppressLint
import android.webkit.JavascriptInterface
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Fullscreen
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.util.MessageGroup

private fun escapeForJsString(s: String): String = buildString(s.length + 16) {
    for (c in s) {
        when (c) {
            '\\' -> append("\\\\")
            '\'' -> append("\\'")
            '"' -> append("\\\"")
            '\n' -> append("\\n")
            '\r' -> append("\\r")
            '\t' -> append("\\t")
            else -> append(c)
        }
    }
}

// Shell HTML loaded into the WebView. Mirrors frontend/src/WidgetCard.tsx SHELL_HTML.
private val SHELL_HTML = """
<!DOCTYPE html>
<html data-theme="light"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com https://esm.sh; style-src 'unsafe-inline'; img-src https: data:; font-src https: data:;">
<script src="https://cdn.jsdelivr.net/npm/morphdom@2/dist/morphdom-umd.min.js"></script>
<style>
:root { color-scheme: light dark; }
body { margin: 0; padding: 8px; background: #fff; color: #1a1a1a; font-family: system-ui, sans-serif; }
._fadeIn { animation: _fadeIn 0.3s ease; }
@keyframes _fadeIn { from { opacity: 0 } to { opacity: 1 } }
</style>
</head><body>
<div id="root"></div>
<script>
window.onerror = function(msg, url, line) {
  _Android.onError(msg + ' (line ' + line + ')');
};
window._setContent = function(html) {
  var root = document.getElementById('root');
  var tmp = document.createElement('div');
  tmp.innerHTML = html;
  if (typeof morphdom !== 'undefined') {
    morphdom(root, tmp, {
      childrenOnly: true,
      onBeforeElUpdated: function(from, to) { return !from.isEqualNode(to); },
      onNodeAdded: function(node) { if (node.classList) node.classList.add('_fadeIn'); return node; }
    });
  } else {
    root.innerHTML = html;
  }
  _Android.onResize(document.body.scrollHeight);
};
window._runScripts = function() {
  document.querySelectorAll('#root script').forEach(function(old) {
    var s = document.createElement('script');
    if (old.src) s.src = old.src;
    else s.textContent = old.textContent;
    old.parentNode.replaceChild(s, old);
  });
};
new ResizeObserver(function() {
  _Android.onResize(document.body.scrollHeight);
}).observe(document.body);
</script>
</body></html>
""".trimIndent()

@SuppressLint("SetJavaScriptEnabled")
@Composable
fun WidgetCard(group: MessageGroup) {
    val density = LocalDensity.current
    var contentHeight by remember { mutableIntStateOf(400) }
    var webViewReady by remember { mutableStateOf(false) }
    var lastPostedHTML by remember { mutableStateOf("") }
    val webViewRef = remember { mutableStateOf<WebView?>(null) }
    var showFullscreen by remember { mutableStateOf(false) }

    val borderColor = MaterialTheme.appColors.widgetBorder
    val bgColor = MaterialTheme.appColors.widgetBg

    // Post content to the WebView when HTML changes or widgetDone transitions.
    LaunchedEffect(group.widgetHTML, group.widgetDone, webViewReady) {
        val html = group.widgetHTML ?: return@LaunchedEffect
        if (!webViewReady) return@LaunchedEffect
        if (html != lastPostedHTML) {
            lastPostedHTML = html
            val escaped = escapeForJsString(html)
            webViewRef.value?.evaluateJavascript("window._setContent('$escaped')", null)
            if (group.widgetDone) {
                webViewRef.value?.evaluateJavascript("window._runScripts()", null)
            }
        } else if (group.widgetDone) {
            // HTML unchanged but just became done — run scripts.
            webViewRef.value?.evaluateJavascript("window._runScripts()", null)
        }
    }

    // Clean up WebView on dispose.
    DisposableEffect(Unit) {
        onDispose {
            webViewRef.value?.destroy()
            webViewRef.value = null
        }
    }

    Surface(
        modifier = Modifier
            .fillMaxWidth()
            .border(1.dp, borderColor, RoundedCornerShape(6.dp)),
        shape = RoundedCornerShape(6.dp),
        color = bgColor,
    ) {
        Column {
            // Header
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(horizontal = 12.dp, vertical = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = group.widgetTitle ?: "Widget",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurface,
                )
                Spacer(modifier = Modifier.width(8.dp))
                Text(
                    text = if (group.widgetDone) "\u2713" else "\u25CF streaming",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(modifier = Modifier.weight(1f))
                IconButton(onClick = { showFullscreen = true }) {
                    Icon(
                        imageVector = Icons.Default.Fullscreen,
                        contentDescription = "Fullscreen",
                        tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            // WebView
            AndroidView(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 100.dp, max = 2000.dp)
                    .padding(horizontal = 4.dp, vertical = 4.dp),
                factory = { context ->
                    WebView(context).apply {
                        settings.javaScriptEnabled = true
                        settings.domStorageEnabled = true
                        addJavascriptInterface(object {
                            @JavascriptInterface
                            fun onResize(heightPx: Int) {
                                val dpHeight = with(density) { heightPx.toDp().value.toInt() + 20 }
                                contentHeight = dpHeight.coerceIn(100, 2000)
                            }

                            @JavascriptInterface
                            @Suppress("unused")
                            fun onError(@Suppress("UNUSED_PARAMETER") message: String) {
                                // Widget script error — logged for debugging.
                            }
                        }, "_Android")
                        webViewClient = object : WebViewClient() {
                            override fun onPageFinished(view: WebView?, url: String?) {
                                webViewReady = true
                                val html = group.widgetHTML
                                if (html != null) {
                                    lastPostedHTML = html
                                    val escaped = escapeForJsString(html)
                                    view?.evaluateJavascript("window._setContent('$escaped')", null)
                                    if (group.widgetDone) {
                                        view?.evaluateJavascript("window._runScripts()", null)
                                    }
                                }
                            }
                        }
                        loadDataWithBaseURL(null, SHELL_HTML, "text/html", "utf-8", null)
                        webViewRef.value = this
                    }
                },
            )
        }
    }

    if (showFullscreen) {
        WidgetFullscreenDialog(
            group = group,
            onDismiss = { showFullscreen = false },
        )
    }
}

@SuppressLint("SetJavaScriptEnabled")
@Composable
private fun WidgetFullscreenDialog(group: MessageGroup, onDismiss: () -> Unit) {
    val density = LocalDensity.current
    var fsWebViewReady by remember { mutableStateOf(false) }
    var fsLastPostedHTML by remember { mutableStateOf("") }
    val fsWebViewRef = remember { mutableStateOf<WebView?>(null) }

    val bgColor = MaterialTheme.appColors.widgetBg

    LaunchedEffect(group.widgetHTML, group.widgetDone, fsWebViewReady) {
        val html = group.widgetHTML ?: return@LaunchedEffect
        if (!fsWebViewReady) return@LaunchedEffect
        if (html != fsLastPostedHTML) {
            fsLastPostedHTML = html
            val escaped = escapeForJsString(html)
            fsWebViewRef.value?.evaluateJavascript("window._setContent('$escaped')", null)
            if (group.widgetDone) {
                fsWebViewRef.value?.evaluateJavascript("window._runScripts()", null)
            }
        } else if (group.widgetDone) {
            fsWebViewRef.value?.evaluateJavascript("window._runScripts()", null)
        }
    }

    DisposableEffect(Unit) {
        onDispose {
            fsWebViewRef.value?.destroy()
            fsWebViewRef.value = null
        }
    }

    Dialog(
        onDismissRequest = onDismiss,
        properties = DialogProperties(usePlatformDefaultWidth = false),
    ) {
        Surface(
            modifier = Modifier.fillMaxSize(),
            color = bgColor,
        ) {
            Column(modifier = Modifier.fillMaxSize()) {
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 12.dp, vertical = 8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text(
                        text = group.widgetTitle ?: "Widget",
                        style = MaterialTheme.typography.titleMedium,
                        color = MaterialTheme.colorScheme.onSurface,
                    )
                    Spacer(modifier = Modifier.width(8.dp))
                    Text(
                        text = if (group.widgetDone) "\u2713" else "\u25CF streaming",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Spacer(modifier = Modifier.weight(1f))
                    IconButton(onClick = onDismiss) {
                        Icon(
                            imageVector = Icons.Default.Close,
                            contentDescription = "Close fullscreen",
                            tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                AndroidView(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(horizontal = 4.dp, vertical = 4.dp),
                    factory = { context ->
                        WebView(context).apply {
                            settings.javaScriptEnabled = true
                            settings.domStorageEnabled = true
                            addJavascriptInterface(object {
                                @JavascriptInterface
                                @Suppress("unused")
                                fun onResize(@Suppress("UNUSED_PARAMETER") heightPx: Int) {
                                    // Fullscreen — no height adjustment needed.
                                }

                                @JavascriptInterface
                                @Suppress("unused")
                                fun onError(@Suppress("UNUSED_PARAMETER") message: String) {
                                    // Widget script error — logged for debugging.
                                }
                            }, "_Android")
                            webViewClient = object : WebViewClient() {
                                override fun onPageFinished(view: WebView?, url: String?) {
                                    fsWebViewReady = true
                                    val html = group.widgetHTML
                                    if (html != null) {
                                        fsLastPostedHTML = html
                                        val escaped = escapeForJsString(html)
                                        view?.evaluateJavascript(
                                            "window._setContent('$escaped')",
                                            null,
                                        )
                                        if (group.widgetDone) {
                                            view?.evaluateJavascript("window._runScripts()", null)
                                        }
                                    }
                                }
                            }
                            loadDataWithBaseURL(null, SHELL_HTML, "text/html", "utf-8", null)
                            fsWebViewRef.value = this
                        }
                    },
                )
            }
        }
    }
}
