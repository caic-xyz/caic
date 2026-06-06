// Unit tests for the process tree builder.
package com.fghbuild.caic.util

import com.caic.sdk.v1.ProcessInfo
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ProcessNodeTest {
    private fun p(pid: Int, ppid: Int, command: String = "cmd") = ProcessInfo(
        pid = pid,
        ppid = ppid,
        user = "user",
        state = "S",
        cpu = 0.0,
        mem = 0.0,
        time = "0:00",
        command = command,
    )

    private fun flatten(nodes: List<ProcessNode>, depth: Int = 0): List<Pair<Int, Int>> {
        val result = mutableListOf<Pair<Int, Int>>()
        for (node in nodes) {
            result.add(node.info.pid to depth)
            result.addAll(flatten(node.children, depth + 1))
        }
        return result
    }

    private fun ids(nodes: List<ProcessNode>): List<Int> = nodes.map { it.info.pid }

    @Test
    fun `empty input returns empty list`() {
        val tree = buildProcessTree(emptyList())
        assertTrue(tree.isEmpty())
    }

    @Test
    fun `single root process with no parent`() {
        val procs = listOf(p(1, 0, "bash"))
        val tree = buildProcessTree(procs)
        assertEquals(1, tree.size)
        assertEquals(1, tree[0].info.pid)
        assertEquals("bash", tree[0].info.command)
        assertTrue(tree[0].children.isEmpty())
    }

    @Test
    fun `multiple root processes`() {
        val procs = listOf(
            p(1, 0, "bash"),
            p(2, 0, "ssh"),
            p(10, 1, "sleep"),
        )
        val tree = buildProcessTree(procs)
        assertEquals(2, tree.size)
        assertEquals(listOf(1, 2), ids(tree).sorted())

        val bash = tree.find { it.info.pid == 1 }!!
        assertEquals(1, bash.children.size)
        assertEquals(10, bash.children[0].info.pid)
        assertEquals("sleep", bash.children[0].info.command)
        assertTrue(bash.children[0].children.isEmpty())

        val ssh = tree.find { it.info.pid == 2 }!!
        assertTrue(ssh.children.isEmpty())
    }

    @Test
    fun `nested parent-child chain`() {
        val procs = listOf(
            p(1, 0, "init"),
            p(10, 1, "bash"),
            p(100, 10, "make"),
            p(1000, 100, "gcc"),
        )
        val tree = buildProcessTree(procs)
        assertEquals(1, tree.size)

        var node = tree[0]
        assertEquals(1, node.info.pid)
        assertEquals(1, node.children.size)

        node = node.children[0]
        assertEquals(10, node.info.pid)
        assertEquals(1, node.children.size)

        node = node.children[0]
        assertEquals(100, node.info.pid)
        assertEquals(1, node.children.size)

        node = node.children[0]
        assertEquals(1000, node.info.pid)
        assertTrue(node.children.isEmpty())
    }

    @Test
    fun `multiple children under same parent`() {
        val procs = listOf(
            p(1, 0, "bash"),
            p(10, 1, "make"),
            p(11, 1, "gcc"),
            p(12, 1, "ld"),
        )
        val tree = buildProcessTree(procs)
        assertEquals(1, tree.size)
        val children = tree[0].children
        assertEquals(3, children.size)
        assertEquals(listOf(10, 11, 12), ids(children).sorted())
    }

    @Test
    fun `ppid outside list makes root`() {
        // ppid 999 is not in the list, so 10 becomes a root.
        val procs = listOf(
            p(10, 999, "orphan"),
            p(11, 10, "child-of-orphan"),
        )
        val tree = buildProcessTree(procs)
        assertEquals(1, tree.size)
        assertEquals(10, tree[0].info.pid)
        assertEquals(1, tree[0].children.size)
        assertEquals(11, tree[0].children[0].info.pid)
    }

    @Test
    fun `preserves all ProcessInfo fields`() {
        val procs = listOf(
            ProcessInfo(
                pid = 5, ppid = 0, user = "root", state = "R",
                cpu = 12.5, mem = 3.2, time = "1:23", command = "myprocess",
            ),
        )
        val tree = buildProcessTree(procs)
        val node = tree[0].info
        assertEquals(5, node.pid)
        assertEquals(0, node.ppid)
        assertEquals("root", node.user)
        assertEquals("R", node.state)
        assertEquals(12.5, node.cpu, 0.0)
        assertEquals(3.2, node.mem, 0.0)
        assertEquals("1:23", node.time)
        assertEquals("myprocess", node.command)
    }

    @Test
    fun `handles unordered input correctly`() {
        // Children listed before parents should still nest correctly.
        val procs = listOf(
            p(100, 10, "gcc"),
            p(10, 1, "make"),
            p(1, 0, "bash"),
            p(11, 1, "ld"),
        )
        val tree = buildProcessTree(procs)
        assertEquals(1, tree.size)
        assertEquals(1, tree[0].info.pid)
        val childIds = ids(tree[0].children).sorted()
        assertEquals(listOf(10, 11), childIds)
        val makeNode = tree[0].children.find { it.info.pid == 10 }!!
        assertEquals(1, makeNode.children.size)
        assertEquals(100, makeNode.children[0].info.pid)
    }

    @Test
    fun `flattens to expected depths`() {
        val procs = listOf(
            p(1, 0, "init"),
            p(2, 1, "daemon"),
            p(3, 2, "worker1"),
            p(4, 2, "worker2"),
            p(5, 0, "other"),
        )
        val flat = flatten(buildProcessTree(procs))
        assertEquals(
            listOf(
                1 to 0, 2 to 1, 3 to 2, 4 to 2, 5 to 0,
            ),
            flat,
        )
    }
}
