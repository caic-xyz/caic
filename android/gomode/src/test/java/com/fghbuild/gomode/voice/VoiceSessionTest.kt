// Unit tests for Go Mode voice service URL resolution.
package com.fghbuild.gomode.voice

import org.junit.Assert.assertEquals
import org.junit.Test

class VoiceSessionTest {
    @Test
    fun resolveServiceURLUsesConfiguredServiceOriginForRelativePaths() {
        assertEquals(
            "http://10.0.2.2:2242/api/caic/v1/mcp",
            VoiceSession.resolveServiceURL(
                baseURL = "http://10.0.2.2:2242/mobile/task/123",
                advertisedURL = "/api/caic/v1/mcp",
            ),
        )
    }

    @Test
    fun resolveServiceURLKeepsAbsoluteGatewayURL() {
        assertEquals(
            "https://voice.example.test",
            VoiceSession.resolveServiceURL(
                baseURL = "http://10.0.2.2:2242",
                advertisedURL = "https://voice.example.test/",
            ),
        )
    }
}
