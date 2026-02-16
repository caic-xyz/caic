// Top-level composable hosting the navigation graph with voice overlay.
package com.fghbuild.caic

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import com.fghbuild.caic.navigation.Screen
import com.fghbuild.caic.ui.settings.SettingsScreen
import com.fghbuild.caic.ui.tasklist.TaskListScreen
import com.fghbuild.caic.voice.VoiceOverlay
import com.fghbuild.caic.voice.VoiceViewModel
import kotlinx.coroutines.launch

@Composable
fun CaicNavGraph(voiceViewModel: VoiceViewModel = hiltViewModel()) {
    val navController = rememberNavController()
    val voiceState by voiceViewModel.voiceState.collectAsStateWithLifecycle()
    val settings by voiceViewModel.settings.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val snackbarHostState = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()

    val permissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        if (granted) {
            voiceViewModel.connect()
        } else {
            scope.launch {
                snackbarHostState.showSnackbar("Microphone permission is required for voice mode")
            }
        }
    }

    LaunchedEffect(Unit) {
        voiceViewModel.setActiveTaskCallback { _ ->
            // Phase 1: no TaskDetail screen yet; log or ignore.
        }
    }

    LaunchedEffect(voiceState.error) {
        val error = voiceState.error ?: return@LaunchedEffect
        snackbarHostState.showSnackbar(error)
    }

    Scaffold(snackbarHost = { SnackbarHost(snackbarHostState) }) { padding ->
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            NavHost(
                navController = navController,
                startDestination = Screen.TaskList.route,
            ) {
                composable(Screen.TaskList.route) {
                    TaskListScreen(
                        onNavigateToSettings = { navController.navigate(Screen.Settings.route) },
                    )
                }
                composable(Screen.Settings.route) {
                    SettingsScreen(
                        onNavigateBack = { navController.popBackStack() },
                    )
                }
            }

            VoiceOverlay(
                voiceState = voiceState,
                voiceEnabled = settings.voiceEnabled,
                onConnect = {
                    if (ContextCompat.checkSelfPermission(
                            context,
                            Manifest.permission.RECORD_AUDIO,
                        ) == PackageManager.PERMISSION_GRANTED
                    ) {
                        voiceViewModel.connect()
                    } else {
                        permissionLauncher.launch(Manifest.permission.RECORD_AUDIO)
                    }
                },
                onStartListening = {
                    if (ContextCompat.checkSelfPermission(
                            context,
                            Manifest.permission.RECORD_AUDIO,
                        ) == PackageManager.PERMISSION_GRANTED
                    ) {
                        voiceViewModel.startListening()
                    } else {
                        permissionLauncher.launch(Manifest.permission.RECORD_AUDIO)
                    }
                },
                onStopListening = { voiceViewModel.stopListening() },
                onDisconnect = { voiceViewModel.disconnect() },
                modifier = Modifier.align(Alignment.BottomCenter),
            )
        }
    }
}
