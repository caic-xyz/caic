// Unit tests for Go Mode service settings fetching and compatibility validation.
package com.fghbuild.gomode.service

import com.fghbuild.gomode.sdk.v1.Settings
import com.fghbuild.gomode.sdk.v1.ToolGroup
import com.fghbuild.gomode.sdk.v1.VoiceGatewaySettings
import com.fghbuild.gomode.sdk.v1.WebShellSettings
import com.fghbuild.gomode.ui.ServiceBootstrapState
import com.fghbuild.gomode.ui.fetchBootstrapState
import kotlinx.coroutines.runBlocking
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ServiceSettingsClientTest {
    @Test
    fun `fetch requests root settings and decodes mcp metadata`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setBody(SETTINGS_JSON).setResponseCode(200))
            val settings = ServiceSettingsClient().fetch(server.url("/nested/path?ignored=1").toString())
            val request = server.takeRequest()

            assertEquals("/.well-known/gomode.json", request.path)
            assertEquals("example-coding-service", settings.service)
            assertEquals(1, settings.apiVersion)
            assertEquals(1, settings.webShell.toolGroups.size)
            assertEquals("/api/service/v1/mcp", settings.webShell.toolGroups[0].endpoint)
            assertEquals("2026-07-28", settings.webShell.toolGroups[0].protocolVersion)
            assertTrue(settings.webShell.toolGroups[0].authRequired)
            assertEquals("/", settings.webShell.voiceGateway.url)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `fetch rejects unsuccessful response`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setResponseCode(500).setBody("oops"))
            val result = runCatching { ServiceSettingsClient().fetch(server.url("/").toString()) }

            assertTrue(result.exceptionOrNull() is ServiceSettingsException)
            assertEquals("Service settings request failed: HTTP 500", result.exceptionOrNull()?.message)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `bootstrap treats auth gated settings as unvalidated`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setResponseCode(401).setBody("sign in"))
            val state = fetchBootstrapState(server.url("/").toString(), ServiceSettingsClient())

            assertTrue(state is ServiceBootstrapState.Unvalidated)
            assertEquals("/.well-known/gomode.json", server.takeRequest().path)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `bootstrap treats html settings response as unvalidated`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setResponseCode(200).setBody("<html>sign in</html>"))
            val state = fetchBootstrapState(server.url("/").toString(), ServiceSettingsClient())

            assertTrue(state is ServiceBootstrapState.Unvalidated)
            assertEquals("/.well-known/gomode.json", server.takeRequest().path)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `bootstrap treats missing required settings field as unvalidated`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            val missingService = SETTINGS_JSON.replace("              \"service\": \"example-coding-service\",\n", "")
            server.enqueue(MockResponse().setResponseCode(200).setBody(missingService))
            val state = fetchBootstrapState(server.url("/").toString(), ServiceSettingsClient())

            assertTrue(state is ServiceBootstrapState.Unvalidated)
            assertEquals("/.well-known/gomode.json", server.takeRequest().path)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `bootstrap treats fetched incompatible settings as error`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            val incompatibleSettings = SETTINGS_JSON.replace("\"apiVersion\": 1", "\"apiVersion\": 2")
            server.enqueue(MockResponse().setResponseCode(200).setBody(incompatibleSettings))
            val state = fetchBootstrapState(server.url("/").toString(), ServiceSettingsClient())

            assertTrue(state is ServiceBootstrapState.Error)
            assertEquals(
                "Unsupported Go Mode service API version 2. This app supports 1.",
                (state as ServiceBootstrapState.Error).message,
            )
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `compatibility accepts matching shell contract`() {
        assertNull(settings().compatibilityError())
    }

    @Test
    fun `compatibility rejects unsupported api version`() {
        val message = settings(apiVersion = 2).compatibilityError()

        assertEquals("Unsupported Go Mode service API version 2. This app supports 1.", message)
    }

    @Test
    fun `compatibility rejects unsupported bridge version`() {
        val message = settings(bridgeVersion = 2).compatibilityError()

        assertEquals("Service requires bridge version 2. This app provides 1.", message)
    }

    @Test
    fun `compatibility rejects malformed required fields`() {
        assertEquals(
            "Service settings field service is required.",
            settings().copy(service = " ").compatibilityError(),
        )

        val settings = settings()
        val group = settings.webShell.toolGroups[0]
        val badEndpoint = settings.copy(
            webShell = settings.webShell.copy(toolGroups = listOf(group.copy(endpoint = "api/service/v1/mcp"))),
        )
        assertEquals(
            "Service settings field webShell.toolGroups[0].endpoint must be an absolute URL or absolute path.",
            badEndpoint.compatibilityError(),
        )

        val missingProtocolVersion = settings.copy(
            webShell = settings.webShell.copy(toolGroups = listOf(group.copy(protocolVersion = ""))),
        )
        assertEquals(
            "Service settings field webShell.toolGroups[0].protocolVersion is required.",
            missingProtocolVersion.compatibilityError(),
        )

        val badSkillURL = settings.copy(
            webShell = settings.webShell.copy(toolGroups = listOf(group.copy(skillUrl = "skills/tasks/SKILL.md"))),
        )
        assertEquals(
            "Service settings field webShell.toolGroups[0].skillUrl must be an absolute URL or absolute path.",
            badSkillURL.compatibilityError(),
        )

        val missingVoiceURL = settings.copy(
            webShell = settings.webShell.copy(
                voiceGateway = VoiceGatewaySettings(required = true),
            ),
        )
        assertEquals(
            "Service settings field webShell.voiceGateway.url is required when voice is required.",
            missingVoiceURL.compatibilityError(),
        )
    }

    private fun settings(
        apiVersion: Int = 1,
        bridgeVersion: Int = 1,
    ): Settings = Settings(
        service = "example-coding-service",
        serviceVersion = "0.10.0",
        apiVersion = apiVersion,
        webShell = WebShellSettings(
            bridgeVersion = bridgeVersion,
            toolGroups = listOf(
                ToolGroup(
                    name = "tasks",
                    endpoint = "/api/service/v1/mcp",
                    protocolVersion = "2026-07-28",
                    authRequired = true,
                ),
            ),
            voiceGateway = VoiceGatewaySettings(
                required = false,
                url = "/",
            ),
        ),
    )

    private companion object {
        const val SETTINGS_JSON = """
            {
              "service": "example-coding-service",
              "serviceVersion": "0.10.0",
              "apiVersion": 1,
              "webShell": {
                "bridgeVersion": 1,
                "toolGroups": [
                  {
                    "name": "tasks",
                    "endpoint": "/api/service/v1/mcp",
                    "protocolVersion": "2026-07-28",
                    "authRequired": true
                  }
                ],
                "voiceGateway": {
                  "required": false,
                  "url": "/",
                  "authRequired": false
                }
              }
            }
        """
    }
}
