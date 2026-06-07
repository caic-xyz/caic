// Minimal backend boundary for WebRTC voice sessions.

package voicertc

import "context"

type backendConnector interface {
	connect(ctx context.Context, sessionID string, sink backendSink) (backendSession, error)
}

type backendSession interface {
	acceptClientMessage(ctx context.Context, data []byte) error
	acceptMicPCM(ctx context.Context, pcm []byte) error
	close() error
}

type backendSink interface {
	backendReady(ctx context.Context)
	sendGatewayMessage(ctx context.Context, data []byte) error
	sendGatewayError(message string)
	cancelSession()
	addAssistantPCM(pcm []byte)
	clearAssistantAudio()
}
