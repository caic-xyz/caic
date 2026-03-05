// Bidirectional map between task IDs and stable 1-based human-friendly numbers.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.Task

class TaskNumberMap {
    private val idToNumber = mutableMapOf<String, Int>()
    private val numberToId = mutableMapOf<Int, String>()
    private var nextNumber = 1

    /** Sync with current task list. Existing tasks keep their number; new tasks get the next. */
    fun update(tasks: List<Task>) {
        val currentIds = tasks.map { it.id }.toSet()
        // Remove stale mappings.
        val stale = idToNumber.keys - currentIds
        for (id in stale) {
            val num = idToNumber.remove(id)
            if (num != null) numberToId.remove(num)
        }
        // Assign numbers to new tasks.
        for (task in tasks) {
            if (task.id !in idToNumber) {
                idToNumber[task.id] = nextNumber
                numberToId[nextNumber] = task.id
                nextNumber++
            }
        }
    }

    /** Clear all mappings and reset the counter. */
    fun reset() {
        idToNumber.clear()
        numberToId.clear()
        nextNumber = 1
    }

    fun toId(number: Int): String? = numberToId[number]

    fun toNumber(id: String): Int? = idToNumber[id]

    fun formatTaskRef(task: Task): String {
        val num = idToNumber[task.id] ?: return task.id
        val shortName = task.title.ifBlank { task.id }
        return "task #$num ($shortName)"
    }
}
