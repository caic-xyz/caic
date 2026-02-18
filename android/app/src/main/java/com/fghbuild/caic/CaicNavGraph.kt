// Top-level composable hosting the navigation graph with voice panel.
package com.fghbuild.caic

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
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
import com.fghbuild.caic.ui.taskdetail.TaskDetailScreen
import com.fghbuild.caic.ui.tasklist.TaskListScreen
import com.fghbuild.caic.voice.VoicePanel
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

    // Track what the mic permission grant should trigger.
    var onMicGranted by remember { mutableStateOf<(() -> Unit)?>(null) }

    val micPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        if (granted) {
            onMicGranted?.invoke()
        } else {
            scope.launch {
                snackbarHostState.showSnackbar("Microphone permission is required for voice mode")
            }
        }
        onMicGranted = null
    }

    val notificationPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { _ -> /* Best-effort; notifications work without it but silently drop. */ }

    LaunchedEffect(Unit) {
        // Request notification permission on first launch.
        if (ContextCompat.checkSelfPermission(
                context,
                Manifest.permission.POST_NOTIFICATIONS,
            ) != PackageManager.PERMISSION_GRANTED
        ) {
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    LaunchedEffect(voiceState.errorId) {
        val error = voiceState.error ?: return@LaunchedEffect
        snackbarHostState.showSnackbar(error)
    }

    Scaffold(
        contentWindowInsets = WindowInsets(0, 0, 0, 0),
        snackbarHost = {
            SnackbarHost(snackbarHostState) { data ->
                Snackbar {
                    Text(
                        text = data.visuals.message,
                        maxLines = 5,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            NavHost(
                navController = navController,
                startDestination = Screen.TaskList.route,
                modifier = Modifier.weight(1f),
            ) {
                composable(Screen.TaskList.route) {
                    TaskListScreen(
                        onNavigateToSettings = { navController.navigate(Screen.Settings.route) },
                        onNavigateToTask = { taskId ->
                            navController.navigate(Screen.TaskDetail(taskId).route)
                        },
                    )
                }
                composable(Screen.Settings.route) {
                    SettingsScreen(
                        onNavigateBack = { navController.popBackStack() },
                    )
                }
                composable(Screen.TaskDetail.ROUTE) { backStackEntry ->
                    val taskId = backStackEntry.arguments?.getString(Screen.TaskDetail.ARG_TASK_ID)
                        ?: return@composable
                    TaskDetailScreen(
                        taskId = taskId,
                        onNavigateBack = { navController.popBackStack() },
                    )
                }
            }

            VoicePanel(
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
                        onMicGranted = { voiceViewModel.connect() }
                        micPermissionLauncher.launch(Manifest.permission.RECORD_AUDIO)
                    }
                },
                onDisconnect = { voiceViewModel.disconnect() },
                onSelectDevice = { voiceViewModel.selectAudioDevice(it) },
                modifier = Modifier.fillMaxWidth(),
            )
        }
    }
}
