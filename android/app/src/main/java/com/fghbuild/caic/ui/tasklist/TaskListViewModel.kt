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
import com.caic.sdk.v1.RepoSpec
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.UserResp
import com.caic.sdk.v1.UsageResp
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskNotifier
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.ui.theme.terminalStates
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

private val naturalChunkRegex = Regex("(\\d+|\\D+)")

private fun naturalCompare(a: String, b: String): Int {
    val ac = naturalChunkRegex.findAll(a).map { it.value }.toList()
    val bc = naturalChunkRegex.findAll(b).map { it.value }.toList()
    for (i in 0 until minOf(ac.size, bc.size)) {
        val cmp = if (ac[i][0].isDigit() && bc[i][0].isDigit()) {
            ac[i].toLong().compareTo(bc[i].toLong())
        } else {
            ac[i].compareTo(bc[i], ignoreCase = true)
        }
        if (cmp != 0) return cmp
    }
    return ac.size.compareTo(bc.size)
}

data class RepoEntry(val path: String, val branch: String)

data class TaskGroup(
    val repo: String,
    val active: List<Task> = emptyList(),
    val stopped: List<Task> = emptyList(),
    val purged: List<Task> = emptyList(),
)

data class TaskListState(
    val tasks: List<Task> = emptyList(),
    val groups: List<TaskGroup> = emptyList(),
    val connected: Boolean = false,
    val serverConfigured: Boolean = false,
    val repos: List<Repo> = emptyList(),
    val harnesses: List<HarnessInfo> = emptyList(),
    val config: Config? = null,
    val usage: UsageResp? = null,
    val selectedRepos: List<RepoEntry> = emptyList(),
    val editingBranches: List<String> = emptyList(),
    val selectedHarness: String = "",
    val selectedModel: String = "",
    val prompt: String = "",
    val recentRepoCount: Int = 0,
    val submitting: Boolean = false,
    val cloning: Boolean = false,
    val error: String? = null,
    val pendingImages: List<ImageData> = emptyList(),
    val supportsImages: Boolean = false,
    val authRequired: Boolean = false,
    val authProviders: List<String> = emptyList(),
    val serverURL: String = "",
    val user: UserResp? = null,
    val availableRecent: List<Repo> = emptyList(),
    val availableRest: List<Repo> = emptyList(),
    val autoFixCI: Boolean = false,
)

@HiltViewModel
class TaskListViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
    private val taskNotifier: TaskNotifier,
) : ViewModel() {

    private val _formState = MutableStateFlow(FormState())

    val state: StateFlow<TaskListState> = combine(
        taskRepository.tasks,
        taskRepository.connected,
        taskRepository.usage,
        settingsRepository.settings,
        _formState,
        settingsRepository.serverPreferences,
    ) { arr ->
        @Suppress("UNCHECKED_CAST")
        val tasks = arr[0] as List<Task>
        val connected = arr[1] as Boolean
        val usage = arr[2] as UsageResp?
        val settings = arr[3] as com.fghbuild.caic.data.SettingsState
        val form = arr[4] as FormState
        val serverPrefs = arr[5] as com.caic.sdk.v1.PreferencesResp?
        val sortedRepos = form.repos.take(form.recentRepoCount).sortedBy { it.path } +
            form.repos.drop(form.recentRepoCount)

        val groupsMap = mutableMapOf<String, TaskGroup>()
        var other = TaskGroup("")

        for (t in tasks) {
            val repoName = t.repos?.firstOrNull()?.name ?: ""
            val g = if (repoName.isNotEmpty()) {
                groupsMap.getOrPut(repoName) { TaskGroup(repoName) }
            } else {
                other
            }

            val nextGroup = when (t.state) {
                "purged", "failed" -> g.copy(purged = g.purged + t)
                "stopped" -> g.copy(stopped = g.stopped + t)
                else -> g.copy(active = g.active + t)
            }

            if (repoName.isNotEmpty()) {
                groupsMap[repoName] = nextGroup
            } else {
                other = nextGroup
            }
        }

        val reposWithTasks = sortedRepos.filter { groupsMap.containsKey(it.path) }
        val sortedGroups = reposWithTasks.mapNotNull { groupsMap[it.path] }.map { g ->
            g.copy(
                active = g.active.sortedWith(
                    Comparator<Task> { a, b ->
                        naturalCompare(
                            a.repos?.firstOrNull()?.branch ?: "",
                            b.repos?.firstOrNull()?.branch ?: "",
                        )
                    }
                ),
                stopped = g.stopped.sortedByDescending { it.id },
                purged = g.purged.sortedByDescending { it.id },
            )
        }.toMutableList()

        if (other.active.isNotEmpty() || other.stopped.isNotEmpty() || other.purged.isNotEmpty()) {
            sortedGroups.add(
                other.copy(
                    active = other.active.sortedByDescending { it.id },
                    stopped = other.stopped.sortedByDescending { it.id },
                    purged = other.purged.sortedByDescending { it.id },
                )
            )
        }

        val selectedPaths = form.selectedRepos.map { it.path }.toSet()
        val recentSlice = sortedRepos.take(form.recentRepoCount)
        val restSlice = sortedRepos.drop(form.recentRepoCount)
        val imgSupport = form.harnesses.any { it.name == form.selectedHarness && it.supportsImages }

        TaskListState(
            tasks = tasks,
            groups = sortedGroups,
            connected = connected,
            serverConfigured = settings.serverURL.isNotBlank(),
            repos = sortedRepos,
            harnesses = form.harnesses,
            config = form.config,
            usage = usage,
            recentRepoCount = form.recentRepoCount,
            selectedRepos = form.selectedRepos,
            editingBranches = form.editingBranches,
            selectedHarness = form.selectedHarness,
            selectedModel = form.selectedModel,
            prompt = form.prompt,
            submitting = form.submitting,
            cloning = form.cloning,
            error = form.error,
            pendingImages = form.pendingImages,
            supportsImages = imgSupport,
            authRequired = form.authRequired,
            authProviders = form.authProviders,
            serverURL = settings.serverURL,
            user = form.user,
            availableRecent = recentSlice.filter { it.path !in selectedPaths },
            availableRest = restSlice.filter { it.path !in selectedPaths },
            autoFixCI = serverPrefs?.settings?.autoFixOnCIFailure == true,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), TaskListState())

    init {
        taskRepository.start(viewModelScope)
        taskNotifier.start(viewModelScope)
        loadFormData()
        observeServerChanges()
    }

    /** Re-runs [loadFormData] when the server URL or auth token changes (e.g. server switch or OAuth). */
    private fun observeServerChanges() {
        viewModelScope.launch {
            settingsRepository.settings
                .map { it.serverURL to it.authToken }
                .distinctUntilChanged()
                .drop(1) // Skip initial value; handled by loadFormData() in init.
                .collect { loadFormData() }
        }
    }

    private fun loadFormData(selectRepo: String? = null) {
        viewModelScope.launch {
            val url = settingsRepository.settings.value.serverURL
            if (url.isBlank()) return@launch
            try {
                val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                // Config is public; fetch it first to detect auth before calling protected endpoints.
                val config = client.getConfig()
                if (config.authProviders?.isNotEmpty() == true) {
                    try {
                        val me = client.getMe()
                        _formState.value = _formState.value.copy(authRequired = false, user = me)
                    } catch (_: Exception) {
                        _formState.value = _formState.value.copy(
                            authRequired = true,
                            authProviders = config.authProviders.orEmpty(),
                        )
                        return@launch
                    }
                }
                val repos = client.listRepos()
                val harnesses = client.listHarnesses()
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
                val initialRepo = selectRepo?.takeIf { path -> repos.any { it.path == path } }
                    ?: ordered.firstOrNull()?.path ?: ""
                _formState.value = _formState.value.copy(
                    repos = ordered,
                    harnesses = harnesses,
                    config = config,
                    recentRepoCount = recentRepos.size,
                    selectedRepos = if (initialRepo.isNotBlank()) listOf(RepoEntry(initialRepo, "")) else emptyList(),
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

    fun addRepo(path: String) {
        val current = _formState.value
        if (current.selectedRepos.any { it.path == path }) return
        _formState.value = current.copy(selectedRepos = current.selectedRepos + RepoEntry(path, ""))
    }

    fun removeRepo(path: String) {
        _formState.value = _formState.value.copy(
            selectedRepos = _formState.value.selectedRepos.filter { it.path != path },
        )
    }

    fun setBranch(path: String, branch: String) {
        _formState.value = _formState.value.copy(
            selectedRepos = _formState.value.selectedRepos.map {
                if (it.path == path) it.copy(branch = branch) else it
            },
        )
    }

    fun loadBranchesForPath(path: String) {
        _formState.value = _formState.value.copy(editingBranches = emptyList())
        if (path.isBlank()) return
        viewModelScope.launch {
            try {
                val url = settingsRepository.settings.value.serverURL
                val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                val resp = client.listRepoBranches(path)
                _formState.value = _formState.value.copy(editingBranches = resp.branches)
            } catch (_: Exception) {
                _formState.value = _formState.value.copy(editingBranches = emptyList())
            }
        }
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
                val client = ApiClient(serverURL, tokenProvider = { settingsRepository.settings.value.authToken })
                val cloned = client.cloneRepo(CloneRepoReq(url = url, path = path?.ifBlank { null }))
                loadFormData(selectRepo = cloned.path)
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
        if (prompt.isBlank()) return
        _formState.value = form.copy(submitting = true, error = null)
        viewModelScope.launch {
            try {
                val url = settingsRepository.settings.value.serverURL
                val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                client.createTask(
                    CreateTaskReq(
                        initialPrompt = Prompt(
                            text = prompt,
                            images = form.pendingImages.ifEmpty { null },
                        ),
                        repos = form.selectedRepos.ifEmpty { null }?.map {
                            RepoSpec(name = it.path, baseBranch = it.branch.ifBlank { null })
                        },
                        harness = form.selectedHarness,
                        model = form.selectedModel.ifBlank { null },
                    )
                )
                // Promote all selected repos to the front of the MRU list.
                val current = _formState.value
                val selectedPaths = form.selectedRepos.map { it.path }.toSet()
                val selectedRepoObjects = form.selectedRepos.mapNotNull { entry ->
                    current.repos.find { it.path == entry.path }
                }
                val oldRecentPaths = current.repos.take(current.recentRepoCount).map { it.path }.toSet()
                val newRecentRepos = selectedRepoObjects +
                    current.repos.take(current.recentRepoCount).filter { it.path !in selectedPaths }
                val reorderedRepos = newRecentRepos + current.repos.filter {
                    it.path !in selectedPaths && it.path !in oldRecentPaths
                }
                val updatedModels = if (form.selectedModel.isNotBlank())
                    current.prefModels + (form.selectedHarness to form.selectedModel)
                else
                    current.prefModels
                _formState.value = current.copy(
                    prompt = "",
                    submitting = false,
                    repos = reorderedRepos,
                    recentRepoCount = newRecentRepos.size,
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

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun fixCI(repoPath: String) {
        val form = _formState.value
        val repo = form.repos.find { it.path == repoPath } ?: return
        val nonPassing = setOf("failure", "cancelled", "timed_out", "action_required", "stale")
        val failing = repo.defaultBranchChecks
            ?.filter { it.conclusion in nonPassing }
            .orEmpty()
        val names = failing.joinToString(", ") { it.name }
        val fixPrompt = buildString {
            append("CI is failing on the default branch of $repoPath.")
            append(" Please fix the failing CI checks and push to the default branch:\n\n")
            append("Failing checks: ${names.ifEmpty { "(unknown)" }}")
        }
        _formState.value = form.copy(submitting = true, error = null)
        viewModelScope.launch {
            try {
                val url = settingsRepository.settings.value.serverURL
                val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                client.createTask(
                    CreateTaskReq(
                        initialPrompt = Prompt(text = fixPrompt),
                        repos = listOf(RepoSpec(name = repoPath)),
                        harness = form.selectedHarness,
                    )
                )
                _formState.value = _formState.value.copy(submitting = false)
            } catch (e: Exception) {
                _formState.value = _formState.value.copy(
                    submitting = false,
                    error = e.message ?: "Failed to create CI fix task",
                )
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Best-effort API call before clearing local state.
    fun logout() {
        viewModelScope.launch {
            try {
                val url = settingsRepository.settings.value.serverURL
                if (url.isNotBlank()) {
                    val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                    client.logout()
                }
            } catch (_: Exception) {
                // Best-effort; continue clearing local state.
            }
            settingsRepository.updateAuthToken(null)
            _formState.value = _formState.value.copy(authRequired = true, user = null)
        }
    }

    private data class FormState(
        val repos: List<Repo> = emptyList(),
        val harnesses: List<HarnessInfo> = emptyList(),
        val config: Config? = null,
        val recentRepoCount: Int = 0,
        val selectedRepos: List<RepoEntry> = emptyList(),
        val editingBranches: List<String> = emptyList(),
        val selectedHarness: String = "",
        val selectedModel: String = "",
        val prompt: String = "",
        val submitting: Boolean = false,
        val cloning: Boolean = false,
        val error: String? = null,
        val pendingImages: List<ImageData> = emptyList(),
        val prefModels: Map<String, String> = emptyMap(),
        val authRequired: Boolean = false,
        val authProviders: List<String> = emptyList(),
        val user: UserResp? = null,
    )
}
