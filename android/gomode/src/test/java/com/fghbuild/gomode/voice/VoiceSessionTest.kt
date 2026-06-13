// Unit tests for Go Mode voice service URL resolution.
package com.fghbuild.gomode.voice

import org.junit.Assert.assertEquals
import org.junit.Test

class VoiceSessionTest {
    @Test
    fun resolveServiceURLUsesConfiguredServiceOriginForRelativePaths() {
        assertEquals(
            "http://10.0.2.2:2242/service/mcp",
            VoiceSession.resolveServiceURL(
                baseURL = "http://10.0.2.2:2242/mobile/screen/123",
                advertisedURL = "/service/mcp",
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
