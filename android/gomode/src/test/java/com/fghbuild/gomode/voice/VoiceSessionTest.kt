// Unit tests for Go Mode voice session diagnostics and service URL resolution.
package com.fghbuild.gomode.voice

import com.caic.voicegateway.sdk.v1.VoiceRTCConnectivityIssue
import com.caic.voicegateway.sdk.v1.VoiceRTCConnectivitySide
import com.caic.voicegateway.sdk.v1.VoiceRTCDiagnosticsResp
import com.caic.voicegateway.sdk.v1.VoiceRTCServerDiagnostics
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class VoiceSessionTest {
    @Test
    fun usableICECandidateAcceptsLANAndTailscaleIPv4UDP() {
        assertTrue(isUsableICECandidate("candidate:1 1 udp 2130706431 192.168.1.64 57033 typ host"))
        assertTrue(isUsableICECandidate("candidate:2 1 udp 2121998079 100.99.136.28 57469 typ host"))
    }

    @Test
    fun usableICECandidateRejectsLinkLocalIPv4AndNonUDPCandidates() {
        assertTrue(!isUsableICECandidate("candidate:1 1 udp 2122260223 169.254.58.144 58981 typ host"))
        assertTrue(!isUsableICECandidate("candidate:2 1 tcp 1518149375 192.168.1.64 9 typ host tcptype active"))
    }

    @Test
    fun summarizeSDPCandidatesReportsCandidateHostPortAndType() {
        val sdp = "v=0\r\n" +
            "a=candidate:1 1 udp 2130706431 70.51.33.231 42602 typ srflx " +
            "raddr 192.168.1.123 rport 42602\r\n" +
            "a=candidate:2 1 udp 2130706431 192.168.1.123 42602 typ host\r\n"

        assertEquals(
            "70.51.33.231:42602 srflx, 192.168.1.123:42602 host",
            summarizeSDPCandidates(sdp),
        )
    }

    @Test
    fun summarizeSDPCandidatesReportsNoneWithoutCandidates() {
        assertEquals("none", summarizeSDPCandidates("v=0\r\n"))
    }

    @Test
    fun formatVoiceRTCDiagnosticsSurfacesUDPMappingError() {
        val message = formatVoiceRTCDiagnostics(
            VoiceRTCDiagnosticsResp(
                sessionID = "voice-session",
                issue = VoiceRTCConnectivityIssue.UDPUnreachable,
                side = VoiceRTCConnectivitySide.Network,
                message = "server is waiting for a WebRTC data channel",
                server = VoiceRTCServerDiagnostics(
                    sessionFound = true,
                    udpMappingError = "refresh UPnP UDP mapping 40000 -> 3478: timeout",
                ),
            ),
        )

        assertTrue(message.contains("UDP mapping: refresh UPnP UDP mapping 40000 -> 3478: timeout"))
    }

    @Test
    fun recoveryPolicyCancelsDisconnectedGraceAndAllowsImmediateFailureRecovery() {
        val policy = VoiceRecoveryPolicy(maxAttempts = 3)

        assertEquals(5_000L, recoveryDelayMs(org.webrtc.PeerConnection.IceConnectionState.DISCONNECTED))
        assertTrue(policy.schedule())
        policy.cancelPending()
        assertFalse(policy.beginScheduledRecovery())

        assertEquals(0L, recoveryDelayMs(org.webrtc.PeerConnection.IceConnectionState.FAILED))
        assertTrue(policy.schedule())
        assertTrue(policy.beginScheduledRecovery())
        assertEquals(1, policy.attempts)
    }

    @Test
    fun recoveryPolicyLimitsRetriesAndManualResetCancelsPendingRecovery() {
        val policy = VoiceRecoveryPolicy(maxAttempts = 3)

        repeat(3) {
            assertTrue(policy.schedule())
            assertTrue(policy.beginScheduledRecovery())
        }
        assertFalse(policy.schedule())

        policy.reset()
        assertTrue(policy.schedule())
        policy.cancelPending()
        assertFalse(policy.beginScheduledRecovery())
        assertEquals(0, policy.attempts)
    }

    @Test
    fun recoveryContextPreservesFinalTranscriptAndBoundsMessage() {
        val context = buildNetworkRecoveryContext(
            listOf(
                TranscriptEntry(TranscriptSpeaker.USER, "first", final = true),
                TranscriptEntry(TranscriptSpeaker.ASSISTANT, "second", final = true),
                TranscriptEntry(TranscriptSpeaker.USER, "partial", final = false),
            ),
            "active service context",
        )

        assertTrue(context.contains("do not treat this as a new user turn"))
        assertTrue(context.contains("Current service context:\nactive service context"))
        assertTrue(context.contains("user: first\nassistant: second"))
        assertFalse(context.contains("partial"))
        assertTrue(context.length <= MAX_RECOVERY_CONTEXT_CHARS)
    }

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
