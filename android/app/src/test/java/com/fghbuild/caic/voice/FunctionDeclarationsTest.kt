// Unit tests for Gemini Live function declaration schema generation.
package com.fghbuild.caic.voice

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class FunctionDeclarationsTest {

    private val noCaps = ServerCaps()

    private val allCaps = ServerCaps(
        tailscaleAvailable = true,
        usbAvailable = true,
        displayAvailable = true,
        sudoAvailable = true,
        gitHubTokenAvailable = true,
    )

    @Test
    fun `returns all 17 function declarations`() {
        val decls = buildFunctionDeclarations(
            harnesses = listOf("claude", "codex"),
            repos = listOf("acme/app"),
            defaultHarness = "claude",
            caps = allCaps,
        )
        assertEquals(17, decls.size)
    }

    @Test
    fun `every declaration has required fields`() {
        val decls = buildFunctionDeclarations(
            harnesses = emptyList(), repos = emptyList(), defaultHarness = null, caps = noCaps,
        )
        for (d in decls) {
            assertTrue("${d.name}: name must be non-blank", d.name.isNotBlank())
            assertTrue("${d.name}: description must be non-blank", d.description.isNotBlank())
            assertTrue("${d.name}: parameters must be a JsonObject", d.parameters is JsonObject)
        }
    }

    @Test
    fun `tasks_list has empty object schema`() {
        val decls = buildFunctionDeclarations(emptyList(), emptyList(), null, noCaps)
        val d = decls.find { it.name == "tasks_list" }!!
        val params = d.parameters as JsonObject
        assertEquals("object", params["type"]?.jsonPrimitive?.content)
        assertEquals(0, (params["properties"] as JsonObject).size)
    }

    @Test
    fun `task_create with empty repos and harnesses uses string params`() {
        val decls = buildFunctionDeclarations(emptyList(), emptyList(), null, noCaps)
        val d = decls.find { it.name == "task_create" }!!
        val params = d.parameters as JsonObject
        val props = params["properties"] as JsonObject

        // repos uses string (no enum) when no repos available
        val reposItems = (props["repos"] as JsonObject)["items"] as JsonObject
        assertEquals("string", reposItems["type"]?.jsonPrimitive?.content)
        assertNull(reposItems["enum"])

        // harness uses string (no enum) when no harnesses available
        val harnessProp = props["harness"] as JsonObject
        assertEquals("string", harnessProp["type"]?.jsonPrimitive?.content)
        assertNull(harnessProp["enum"])
    }

    @Test
    fun `task_create with repos uses enum items`() {
        val decls = buildFunctionDeclarations(
            harnesses = emptyList(),
            repos = listOf("acme/web", "acme/api"),
            defaultHarness = null,
            caps = noCaps,
        )
        val d = decls.find { it.name == "task_create" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val reposItems = (props["repos"] as JsonObject)["items"] as JsonObject
        assertEquals("string", reposItems["type"]?.jsonPrimitive?.content)
        val enum = reposItems["enum"] as JsonArray
        assertEquals(listOf("acme/web", "acme/api"), enum.map { it.jsonPrimitive.content })
    }

    @Test
    fun `task_create with harnesses uses enum`() {
        val decls = buildFunctionDeclarations(
            harnesses = listOf("claude", "gemini", "codex"),
            repos = emptyList(),
            defaultHarness = null,
            caps = noCaps,
        )
        val d = decls.find { it.name == "task_create" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val harnessProp = props["harness"] as JsonObject
        assertEquals("string", harnessProp["type"]?.jsonPrimitive?.content)
        val enum = harnessProp["enum"] as JsonArray
        assertEquals(listOf("claude", "gemini", "codex"), enum.map { it.jsonPrimitive.content })
    }

    @Test
    fun `task_create required fields are prompt and repos`() {
        val decls = buildFunctionDeclarations(emptyList(), emptyList(), null, noCaps)
        val d = decls.find { it.name == "task_create" }!!
        val params = d.parameters as JsonObject
        val required = params["required"] as JsonArray
        assertEquals(listOf("prompt", "repos"), required.map { it.jsonPrimitive.content })
    }

    @Test
    fun `harness description includes default when set`() {
        val decls = buildFunctionDeclarations(
            harnesses = listOf("claude", "codex"),
            repos = emptyList(), defaultHarness = "codex", caps = noCaps,
        )
        val d = decls.find { it.name == "task_create" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val desc = (props["harness"] as JsonObject)["description"]?.jsonPrimitive?.content
        assertNotNull(desc)
        assertTrue(desc!!.contains("default: codex"))
    }

    @Test
    fun `harness description is generic when no default`() {
        val decls = buildFunctionDeclarations(
            harnesses = emptyList(), repos = emptyList(), defaultHarness = null, caps = noCaps,
        )
        val d = decls.find { it.name == "task_create" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val desc = (props["harness"] as JsonObject)["description"]?.jsonPrimitive?.content
        assertNotNull(desc)
        assertTrue(desc!!.contains("optional"))
    }

    @Test
    fun `capability flags affect prop descriptions`() {
        // With capabilities enabled: "Enable virtual display (VNC) for this task"
        // Without: "Enable virtual display (VNC) for this task (not available on this server)"
        val enabled = buildFunctionDeclarations(
            harnesses = emptyList(), repos = emptyList(), defaultHarness = null, caps = allCaps,
        )
        val disabled = buildFunctionDeclarations(
            harnesses = emptyList(), repos = emptyList(), defaultHarness = null, caps = noCaps,
        )

        fun getDesc(decls: List<FunctionDeclaration>, name: String): String {
            val d = decls.find { it.name == "task_create" }!!
            val props = (d.parameters as JsonObject)["properties"] as JsonObject
            return (props[name] as JsonObject)["description"]?.jsonPrimitive?.content ?: ""
        }

        assertTrue("display enabled", !getDesc(enabled, "display").contains("not available"))
        assertTrue("display disabled", getDesc(disabled, "display").contains("not available"))
        assertTrue("tailscale enabled", !getDesc(enabled, "tailscale").contains("not available"))
        assertTrue("tailscale disabled", getDesc(disabled, "tailscale").contains("not available"))
    }

    @Test
    fun `bot_fix_ci with repos uses enum`() {
        val decls = buildFunctionDeclarations(
            harnesses = emptyList(), repos = listOf("org/a", "org/b"), defaultHarness = null, caps = noCaps,
        )
        val d = decls.find { it.name == "bot_fix_ci" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val repoProp = props["repo"] as JsonObject
        assertEquals("string", repoProp["type"]?.jsonPrimitive?.content)
        assertEquals(listOf("org/a", "org/b"), (repoProp["enum"] as JsonArray).map { it.jsonPrimitive.content })
    }

    @Test
    fun `bot_fix_ci without repos uses string`() {
        val decls = buildFunctionDeclarations(
            harnesses = emptyList(), repos = emptyList(), defaultHarness = null, caps = noCaps,
        )
        val d = decls.find { it.name == "bot_fix_ci" }!!
        val props = (d.parameters as JsonObject)["properties"] as JsonObject
        val repoProp = props["repo"] as JsonObject
        assertEquals("string", repoProp["type"]?.jsonPrimitive?.content)
        assertNull(repoProp["enum"])
    }

    @Test
    fun `all declarations have unique names`() {
        val decls = buildFunctionDeclarations(emptyList(), emptyList(), null, noCaps)
        val names = decls.map { it.name }
        assertEquals(names.size, names.toSet().size)
    }

    @Test
    fun `task_number declarations have required task_number field`() {
        val decls = buildFunctionDeclarations(emptyList(), emptyList(), null, noCaps)
        val taskNumberFuncs = listOf(
            "task_get_detail", "task_send_message", "task_answer_question",
            "task_push_branch_to_remote", "task_stop", "task_purge",
            "task_revive", "task_fork", "agent_last_message", "task_fix_pr",
        )
        for (name in taskNumberFuncs) {
            val d = decls.find { it.name == name }
            assertNotNull("$name must exist", d)
            val params = d!!.parameters as JsonObject
            val required = params["required"] as JsonArray
            assertTrue("$name requires task_number", required.any { it.jsonPrimitive.content == "task_number" })
        }
    }
}
