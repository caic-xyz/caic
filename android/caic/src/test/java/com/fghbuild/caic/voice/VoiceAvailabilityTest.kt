// Unit tests for server voice availability gating.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.Config
import com.caic.sdk.v1.VoiceGatewayMetadata
import com.caic.sdk.v1.VoiceGatewayMode
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class VoiceAvailabilityTest {

    @Test
    fun `voice is unavailable until server config is known`() {
        assertFalse(isVoiceAvailable(null))
    }

    @Test
    fun `voice is unavailable when server disables gateway`() {
        assertFalse(isVoiceAvailable(serverConfig(VoiceGatewayMode.Disabled)))
    }

    @Test
    fun `voice is available when server enables gateway`() {
        assertTrue(isVoiceAvailable(serverConfig(VoiceGatewayMode.Embedded)))
    }

    private fun serverConfig(mode: VoiceGatewayMode): Config = Config(
        displayName = "test",
        tailscaleAvailable = false,
        usbAvailable = false,
        displayAvailable = false,
        sudoAvailable = false,
        gitHubTokenAvailable = false,
        voiceGateway = VoiceGatewayMetadata(mode = mode),
    )
}
