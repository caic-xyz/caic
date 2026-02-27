// Top-level composable hosting the navigation graph with voice panel.
package com.fghbuild.caic

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.VerticalDivider
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import com.fghbuild.caic.navigation.Screen
import com.fghbuild.caic.ui.diff.DiffScreen
import com.fghbuild.caic.ui.settings.SettingsScreen
import com.fghbuild.caic.ui.taskdetail.TaskDetailScreen
import com.fghbuild.caic.ui.tasklist.TaskListScreen
import com.fghbuild.caic.voice.VoicePanel
import com.fghbuild.caic.voice.VoiceViewModel
import kotlinx.coroutines.launch

private val WideBreakpoint = 840.dp

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
            BoxWithConstraints(modifier = Modifier.weight(1f)) {
                val wide = maxWidth >= WideBreakpoint
                if (wide) {
                    WideLayout(
                        navController = navController,
                        modifier = Modifier.fillMaxSize(),
                    )
                } else {
                    CompactLayout(
                        navController = navController,
                        modifier = Modifier.fillMaxSize(),
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
                onToggleMute = { voiceViewModel.toggleMute() },
                onSelectDevice = { voiceViewModel.selectAudioDevice(it) },
                modifier = Modifier.fillMaxWidth(),
            )
        }
    }
}

/** Narrow layout: single-pane navigation with all three screens in the NavHost. */
@Composable
private fun CompactLayout(
    navController: NavHostController,
    modifier: Modifier = Modifier,
) {
    NavHost(
        navController = navController,
        startDestination = Screen.TaskList.route,
        modifier = modifier,
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
                onNavigateToDiff = {
                    navController.navigate(Screen.TaskDiff(taskId).route)
                },
            )
        }
        composable(Screen.TaskDiff.ROUTE) { backStackEntry ->
            val taskId = backStackEntry.arguments?.getString(Screen.TaskDiff.ARG_TASK_ID)
                ?: return@composable
            DiffScreen(
                taskId = taskId,
                onNavigateBack = { navController.popBackStack() },
            )
        }
    }
}

/** Wide layout: task list pinned on the left, detail/settings on the right. */
@Composable
private fun WideLayout(
    navController: NavHostController,
    modifier: Modifier = Modifier,
) {
    Row(modifier = modifier) {
        TaskListScreen(
            onNavigateToSettings = {
                navController.navigate(Screen.Settings.route) {
                    popUpTo(Screen.TaskList.route)
                }
            },
            onNavigateToTask = { taskId ->
                navController.navigate(Screen.TaskDetail(taskId).route) {
                    popUpTo(Screen.TaskList.route)
                }
            },
            modifier = Modifier
                .width(360.dp)
                .fillMaxHeight(),
        )
        VerticalDivider()
        NavHost(
            navController = navController,
            startDestination = Screen.TaskList.route,
            modifier = Modifier
                .weight(1f)
                .fillMaxHeight(),
        ) {
            composable(Screen.TaskList.route) {
                // Placeholder when no task is selected in wide mode.
                Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("Select a task", style = MaterialTheme.typography.bodyLarge)
                }
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
                    onNavigateToDiff = {
                        navController.navigate(Screen.TaskDiff(taskId).route) {
                            popUpTo(Screen.TaskList.route)
                        }
                    },
                )
            }
            composable(Screen.TaskDiff.ROUTE) { backStackEntry ->
                val taskId = backStackEntry.arguments?.getString(Screen.TaskDiff.ARG_TASK_ID)
                    ?: return@composable
                DiffScreen(
                    taskId = taskId,
                    onNavigateBack = { navController.popBackStack() },
                )
            }
        }
    }
}
