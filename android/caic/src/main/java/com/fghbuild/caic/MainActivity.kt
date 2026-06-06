// Single activity host for Jetpack Compose UI.
package com.fghbuild.caic

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.fghbuild.caic.data.AuthTokenStore
import com.fghbuild.caic.ui.theme.CaicTheme
import dagger.hilt.android.AndroidEntryPoint
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import javax.inject.Inject

@AndroidEntryPoint
class MainActivity : ComponentActivity() {
    @Inject lateinit var authTokenStore: AuthTokenStore

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        handleAuthIntent(intent)
        setContent {
            CaicTheme {
                CaicNavGraph()
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleAuthIntent(intent)
    }

    private fun handleAuthIntent(intent: Intent) {
        val uri = intent.data ?: return
        if (uri.scheme == "caic" && uri.host == "auth") {
            val token = uri.getQueryParameter("token") ?: return
            CoroutineScope(Dispatchers.IO).launch {
                authTokenStore.setToken(token)
            }
        }
    }
}
