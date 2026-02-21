// ViewModel for the task list screen: SSE tasks, usage, creation form, and config.
package com.fghbuild.caic.ui.tasklist

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.Config
import com.caic.sdk.v1.CreateTaskReq
import com.caic.sdk.v1.HarnessInfo
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.Repo
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.UsageResp
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
    val tasks: List<Task> = emptyList(),
    val connected: Boolean = false,
    val serverConfigured: Boolean = false,
    val repos: List<Repo> = emptyList(),
    val harnesses: List<HarnessInfo> = emptyList(),
    val config: Config? = null,
    val usage: UsageResp? = null,
    val selectedRepo: String = "",
    val selectedHarness: String = "claude",
    val selectedModel: String = "",
    val prompt: String = "",
    val recentRepoCount: Int = 0,
    val submitting: Boolean = false,
    val error: String? = null,
    val pendingImages: List<ImageData> = emptyList(),
    val supportsImages: Boolean = false,
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
            compareByDescending<Task> { it.state in activeStates }
                .thenByDescending { it.id }
        )
        val imgSupport = form.harnesses
            .any { it.name == form.selectedHarness && it.supportsImages }
        TaskListState(
            tasks = sorted,
            connected = connected,
            serverConfigured = settings.serverURL.isNotBlank(),
            repos = form.repos,
            harnesses = form.harnesses,
            config = form.config,
            usage = usage,
            recentRepoCount = form.recentRepoCount,
            selectedRepo = form.selectedRepo,
            selectedHarness = form.selectedHarness,
            selectedModel = form.selectedModel,
            prompt = form.prompt,
            submitting = form.submitting,
            error = form.error,
            pendingImages = form.pendingImages,
            supportsImages = imgSupport,
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
                val recent = settingsRepository.settings.value.recentRepos
                val recentSet = recent.toSet()
                val recentRepos = recent.mapNotNull { r -> repos.find { it.path == r } }
                val restRepos = repos.filter { it.path !in recentSet }
                val ordered = recentRepos + restRepos
                val selectedHarness = _formState.value.selectedHarness
                val lastModels = settingsRepository.settings.value.lastModels
                val lastModel = lastModels[selectedHarness] ?: ""
                val harnessModels = harnesses.find { it.name == selectedHarness }?.models.orEmpty()
                _formState.value = _formState.value.copy(
                    repos = ordered,
                    harnesses = harnesses,
                    config = config,
                    recentRepoCount = recentRepos.size,
                    selectedRepo = ordered.firstOrNull()?.path ?: "",
                    selectedModel = if (lastModel in harnessModels) lastModel else "",
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
        val lastModels = settingsRepository.settings.value.lastModels
        val lastModel = lastModels[harness] ?: ""
        val harnessModels = _formState.value.harnesses.find { it.name == harness }?.models.orEmpty()
        val model = if (lastModel in harnessModels) lastModel else ""
        _formState.value = _formState.value.copy(selectedHarness = harness, selectedModel = model)
    }

    fun selectModel(model: String) {
        val harness = _formState.value.selectedHarness
        _formState.value = _formState.value.copy(selectedModel = model)
        viewModelScope.launch {
            settingsRepository.updateLastModel(harness, model)
        }
    }

    fun addImages(images: List<ImageData>) {
        _formState.value = _formState.value.copy(
            pendingImages = _formState.value.pendingImages + images,
        )
    }

    fun removeImage(index: Int) {
        _formState.value = _formState.value.copy(
            pendingImages = _formState.value.pendingImages.filterIndexed { i, _ -> i != index },
        )
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
                        initialPrompt = Prompt(
                            text = prompt,
                            images = form.pendingImages.ifEmpty { null },
                        ),
                        repo = form.selectedRepo,
                        harness = form.selectedHarness,
                        model = form.selectedModel.ifBlank { null },
                    )
                )
                settingsRepository.addRecentRepo(form.selectedRepo)
                val current = _formState.value
                val recent = settingsRepository.settings.value.recentRepos
                val recentSet = recent.toSet()
                val recentRepos = recent.mapNotNull { r -> current.repos.find { it.path == r } }
                val restRepos = current.repos.filter { it.path !in recentSet }
                _formState.value = current.copy(
                    prompt = "",
                    submitting = false,
                    repos = recentRepos + restRepos,
                    recentRepoCount = recentRepos.size,
                    pendingImages = emptyList(),
                )
            } catch (e: Exception) {
                _formState.value = _formState.value.copy(
                    submitting = false,
                    error = e.message ?: "Failed to create task",
                )
            }
        }
    }

    private data class FormState(
        val repos: List<Repo> = emptyList(),
        val harnesses: List<HarnessInfo> = emptyList(),
        val config: Config? = null,
        val recentRepoCount: Int = 0,
        val selectedRepo: String = "",
        val selectedHarness: String = "claude",
        val selectedModel: String = "",
        val prompt: String = "",
        val submitting: Boolean = false,
        val error: String? = null,
        val pendingImages: List<ImageData> = emptyList(),
    )
}
