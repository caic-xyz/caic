// ViewModel for the task list screen: SSE tasks, usage, creation form, and config.
package com.fghbuild.caic.ui.tasklist

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CloneRepoReq
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
    val selectedHarness: String = "",
    val selectedModel: String = "",
    val baseBranch: String = "",
    val prompt: String = "",
    val recentRepoCount: Int = 0,
    val submitting: Boolean = false,
    val cloning: Boolean = false,
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
            baseBranch = form.baseBranch,
            prompt = form.prompt,
            submitting = form.submitting,
            cloning = form.cloning,
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
                val prefs = try {
                    client.getPreferences().also { settingsRepository.updateServerPreferences(it) }
                } catch (_: Exception) { null }
                val recentPaths = prefs?.repositories?.map { it.path }.orEmpty()
                val recentSet = recentPaths.toSet()
                val recentRepos = recentPaths.mapNotNull { r -> repos.find { it.path == r } }
                val restRepos = repos.filter { it.path !in recentSet }
                val ordered = recentRepos + restRepos
                val prefModels = prefs?.models.orEmpty()
                val prefHarness = prefs?.harness ?: ""
                val selectedHarness = if (prefHarness.isNotBlank() && harnesses.any { it.name == prefHarness })
                    prefHarness
                else
                    harnesses.firstOrNull()?.name ?: ""
                val lastModel = prefModels[selectedHarness] ?: ""
                val harnessModels = harnesses.find { it.name == selectedHarness }?.models.orEmpty()
                _formState.value = _formState.value.copy(
                    repos = ordered,
                    harnesses = harnesses,
                    config = config,
                    recentRepoCount = recentRepos.size,
                    selectedRepo = ordered.firstOrNull()?.path ?: "",
                    selectedHarness = selectedHarness,
                    selectedModel = if (lastModel in harnessModels) lastModel else "",
                    prefModels = prefModels,
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
        val lastModel = _formState.value.prefModels[harness] ?: ""
        val harnessModels = _formState.value.harnesses.find { it.name == harness }?.models.orEmpty()
        val model = if (lastModel in harnessModels) lastModel else ""
        _formState.value = _formState.value.copy(selectedHarness = harness, selectedModel = model)
    }

    fun selectModel(model: String) {
        val harness = _formState.value.selectedHarness
        val updated = if (model.isBlank())
            _formState.value.prefModels - harness
        else
            _formState.value.prefModels + (harness to model)
        _formState.value = _formState.value.copy(selectedModel = model, prefModels = updated)
    }

    fun updateBaseBranch(branch: String) {
        _formState.value = _formState.value.copy(baseBranch = branch)
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
    fun cloneRepo(url: String, path: String?) {
        if (url.isBlank()) return
        _formState.value = _formState.value.copy(cloning = true, error = null)
        viewModelScope.launch {
            try {
                val serverURL = settingsRepository.settings.value.serverURL
                val client = ApiClient(serverURL)
                client.cloneRepo(CloneRepoReq(url = url, path = path?.ifBlank { null }))
                loadFormData()
                _formState.value = _formState.value.copy(cloning = false)
            } catch (e: Exception) {
                _formState.value = _formState.value.copy(
                    cloning = false,
                    error = e.message ?: "Failed to clone repository",
                )
            }
        }
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
                        baseBranch = form.baseBranch.ifBlank { null },
                        harness = form.selectedHarness,
                        model = form.selectedModel.ifBlank { null },
                    )
                )
                // Optimistic reorder: move the selected repo to the front.
                val current = _formState.value
                val idx = current.repos.indexOfFirst { it.path == form.selectedRepo }
                val reorderedRepos = if (idx > 0) {
                    val before = current.repos.subList(0, idx)
                    val after = current.repos.subList(idx + 1, current.repos.size)
                    listOf(current.repos[idx]) + before + after
                } else {
                    current.repos
                }
                val newRecentCount = if (idx > current.recentRepoCount - 1)
                    (current.recentRepoCount + 1).coerceAtMost(current.repos.size)
                else
                    current.recentRepoCount
                val updatedModels = if (form.selectedModel.isNotBlank())
                    current.prefModels + (form.selectedHarness to form.selectedModel)
                else
                    current.prefModels
                _formState.value = current.copy(
                    prompt = "",
                    baseBranch = "",
                    submitting = false,
                    repos = reorderedRepos,
                    recentRepoCount = newRecentCount,
                    pendingImages = emptyList(),
                    prefModels = updatedModels,
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
        val selectedHarness: String = "",
        val selectedModel: String = "",
        val baseBranch: String = "",
        val prompt: String = "",
        val submitting: Boolean = false,
        val cloning: Boolean = false,
        val error: String? = null,
        val pendingImages: List<ImageData> = emptyList(),
        val prefModels: Map<String, String> = emptyMap(),
    )
}
