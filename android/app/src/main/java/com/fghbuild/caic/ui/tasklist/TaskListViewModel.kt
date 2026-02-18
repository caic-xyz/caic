// ViewModel for the task list screen: SSE tasks, usage, creation form, and config.
package com.fghbuild.caic.ui.tasklist

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.ApiClient
import com.caic.sdk.ConfigJSON
import com.caic.sdk.CreateTaskReq
import com.caic.sdk.HarnessJSON
import com.caic.sdk.RepoJSON
import com.caic.sdk.TaskJSON
import com.caic.sdk.UsageResp
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.ui.theme.activeStates
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

data class TaskListState(
    val tasks: List<TaskJSON> = emptyList(),
    val connected: Boolean = false,
    val serverConfigured: Boolean = false,
    val repos: List<RepoJSON> = emptyList(),
    val harnesses: List<HarnessJSON> = emptyList(),
    val config: ConfigJSON? = null,
    val usage: UsageResp? = null,
    val selectedRepo: String = "",
    val selectedHarness: String = "claude",
    val selectedModel: String = "",
    val prompt: String = "",
    val submitting: Boolean = false,
    val error: String? = null,
)

@HiltViewModel
class TaskListViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
) : ViewModel() {

    private val _formState = MutableStateFlow(FormState())

    val state: StateFlow<TaskListState> = combine(
        taskRepository.tasks,
        taskRepository.connected,
        taskRepository.usage,
        settingsRepository.settings,
        _formState,
    ) { tasks, connected, usage, settings, form ->
        val sorted = tasks.sortedWith(
            compareByDescending<TaskJSON> { it.state in activeStates }
                .thenByDescending { it.id }
        )
        TaskListState(
            tasks = sorted,
            connected = connected,
            serverConfigured = settings.serverURL.isNotBlank(),
            repos = form.repos,
            harnesses = form.harnesses,
            config = form.config,
            usage = usage,
            selectedRepo = form.selectedRepo,
            selectedHarness = form.selectedHarness,
            selectedModel = form.selectedModel,
            prompt = form.prompt,
            submitting = form.submitting,
            error = form.error,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), TaskListState())

    init {
        taskRepository.start(viewModelScope)
        loadFormData()
    }

    private fun loadFormData() {
        viewModelScope.launch {
            val url = settingsRepository.settings.value.serverURL
            if (url.isBlank()) return@launch
            try {
                val client = ApiClient(url)
                val repos = client.listRepos()
                val harnesses = client.listHarnesses()
                val config = client.getConfig()
                _formState.value = _formState.value.copy(
                    repos = repos,
                    harnesses = harnesses,
                    config = config,
                    selectedRepo = repos.firstOrNull()?.path ?: "",
                )
            } catch (_: Exception) {
                // Form data will remain empty; user can still see tasks.
            }
        }
    }

    fun updatePrompt(text: String) {
        _formState.value = _formState.value.copy(prompt = text)
    }

    fun selectRepo(repo: String) {
        _formState.value = _formState.value.copy(selectedRepo = repo)
    }

    fun selectHarness(harness: String) {
        _formState.value = _formState.value.copy(selectedHarness = harness, selectedModel = "")
    }

    fun selectModel(model: String) {
        _formState.value = _formState.value.copy(selectedModel = model)
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun createTask() {
        val form = _formState.value
        val prompt = form.prompt.trim()
        if (prompt.isBlank() || form.selectedRepo.isBlank()) return
        _formState.value = form.copy(submitting = true, error = null)
        viewModelScope.launch {
            try {
                val url = settingsRepository.settings.value.serverURL
                val client = ApiClient(url)
                client.createTask(
                    CreateTaskReq(
                        prompt = prompt,
                        repo = form.selectedRepo,
                        harness = form.selectedHarness,
                        model = form.selectedModel.ifBlank { null },
                    )
                )
                _formState.value = _formState.value.copy(prompt = "", submitting = false)
            } catch (e: Exception) {
                _formState.value = _formState.value.copy(
                    submitting = false,
                    error = e.message ?: "Failed to create task",
                )
            }
        }
    }

    private data class FormState(
        val repos: List<RepoJSON> = emptyList(),
        val harnesses: List<HarnessJSON> = emptyList(),
        val config: ConfigJSON? = null,
        val selectedRepo: String = "",
        val selectedHarness: String = "claude",
        val selectedModel: String = "",
        val prompt: String = "",
        val submitting: Boolean = false,
        val error: String? = null,
    )
}
