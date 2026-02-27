// ViewModel for the diff screen: fetches full diff once, splits by file.
package com.fghbuild.caic.ui.diff

import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.DiffFileStat
import com.caic.sdk.v1.Task
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.navigation.Screen
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

/** One file's portion of the unified diff. */
data class FileDiff(val path: String, val content: String)

data class DiffState(
    val task: Task? = null,
    val files: List<DiffFileStat> = emptyList(),
    val fileDiffs: List<FileDiff> = emptyList(),
    val collapsedFiles: Set<String> = emptySet(),
    val loading: Boolean = true,
    val error: String? = null,
)

@HiltViewModel
class DiffViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    savedStateHandle: SavedStateHandle,
) : ViewModel() {

    private val taskId: String =
        savedStateHandle[Screen.TaskDiff.ARG_TASK_ID] ?: ""

    private val _fileDiffs = MutableStateFlow<List<FileDiff>>(emptyList())
    private val _collapsedFiles = MutableStateFlow<Set<String>>(emptySet())
    private val _loading = MutableStateFlow(true)
    private val _error = MutableStateFlow<String?>(null)

    @Suppress("UNCHECKED_CAST")
    val state: StateFlow<DiffState> = combine(
        listOf(
            taskRepository.tasks,
            _fileDiffs,
            _collapsedFiles,
            _loading,
            _error,
        ),
    ) { values ->
        val tasks = values[0] as List<Task>
        val diffs = values[1] as List<FileDiff>
        val collapsed = values[2] as Set<String>
        val loading = values[3] as Boolean
        val error = values[4] as String?
        val task = tasks.firstOrNull { it.id == taskId }
        DiffState(
            task = task,
            files = task?.diffStat.orEmpty(),
            fileDiffs = diffs,
            collapsedFiles = collapsed,
            loading = loading,
            error = error,
        )
    }.stateIn(
        viewModelScope,
        SharingStarted.WhileSubscribed(5000),
        DiffState(),
    )

    init {
        fetchFullDiff()
    }

    fun toggleFile(path: String) {
        val current = _collapsedFiles.value
        _collapsedFiles.value = if (path in current) {
            current - path
        } else {
            current + path
        }
    }

    @Suppress("TooGenericExceptionCaught")
    private fun fetchFullDiff() {
        viewModelScope.launch {
            try {
                val baseURL = taskRepository.serverURL()
                if (baseURL.isBlank()) return@launch
                val client = ApiClient(baseURL)
                val resp = client.getTaskDiff(taskId)
                _fileDiffs.value = splitDiff(resp.diff)
            } catch (e: Exception) {
                _error.value = e.message ?: "Unknown error"
            } finally {
                _loading.value = false
            }
        }
    }

    companion object {
        private val DIFF_HEADER = Regex("^(?=diff --git )", RegexOption.MULTILINE)
        // +++ b/path or +++ path (most reliable source).
        private val PLUS_RE = Regex("^\\+\\+\\+ (?:[a-z]/)?(.+)", RegexOption.MULTILINE)
        // --- a/path for deleted files.
        private val MINUS_RE = Regex("^--- (?:[a-z]/)?(.+)", RegexOption.MULTILINE)

        /** Extract the file path from a single diff section. */
        private fun extractPath(section: String): String {
            PLUS_RE.find(section)?.groupValues?.get(1)
                ?.takeIf { it != "/dev/null" }?.let { return it }
            MINUS_RE.find(section)?.groupValues?.get(1)
                ?.takeIf { it != "/dev/null" }?.let { return it }
            return "unknown"
        }

        /** Split a unified diff into per-file sections. */
        fun splitDiff(raw: String): List<FileDiff> {
            if (raw.isBlank()) return emptyList()
            return raw.split(DIFF_HEADER)
                .filter { it.isNotBlank() }
                .map { part -> FileDiff(extractPath(part), part) }
        }
    }
}
