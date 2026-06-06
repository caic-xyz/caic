// Composable helper to request POST_NOTIFICATIONS permission on first user action.
package com.fghbuild.caic.ui.common

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat

/** Returns a callback that requests notification permission if not yet granted. Idempotent. */
@Composable
fun rememberNotificationPermissionRequester(): () -> Unit {
    val context = LocalContext.current
    val launcher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { _ -> /* Best-effort; notifications work without it but silently drop. */ }
    return {
        if (ContextCompat.checkSelfPermission(
                context,
                Manifest.permission.POST_NOTIFICATIONS,
            ) != PackageManager.PERMISSION_GRANTED
        ) {
            launcher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }
}
