// Builds a process tree from a flat process list using pid/ppid relationships.
package com.fghbuild.caic.util

import com.caic.sdk.v1.ProcessInfo

/** Node in a process tree built from a flat process list. */
data class ProcessNode(
    val info: ProcessInfo,
    val children: List<ProcessNode>,
)

/** Builds a tree from a flat process list using pid/ppid relationships.
 * Roots are processes whose PPID is not found in the flat list. */
fun buildProcessTree(procs: List<ProcessInfo>): List<ProcessNode> {
    val byPID = procs.associateBy { it.pid }
    val children = mutableMapOf<Int, MutableList<ProcessInfo>>()
    for (p in procs) {
        children.getOrPut(p.ppid) { mutableListOf() }.add(p)
    }
    fun build(info: ProcessInfo): ProcessNode {
        val kids = children[info.pid]?.map { build(it) } ?: emptyList()
        return ProcessNode(info, kids)
    }
    // Roots are processes whose PPID is not in the flat list.
    return procs.filter { it.ppid !in byPID }.map { build(it) }
}
