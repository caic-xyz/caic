// Stubs for the WebRTC voice bridge when built with the race detector.
// The real voicertc package depends on gopus which is incompatible with -race.

//go:build race

package voicertc

import (
	"context"
	"errors"
)

// Bridge is a no-op stub when building with -race.
type Bridge struct{}

// NewBridge returns an error when built with -race.
func NewBridge(_ context.Context, _ string, _ int) (*Bridge, error) {
	return nil, errors.New("voicertc is not available in race builds")
}

// HandleOffer returns an error when built with -race.
func (b *Bridge) HandleOffer(_ context.Context, _ string) (string, string, error) {
	return "", "", errors.New("voicertc is not available in race builds")
}

// Close is a no-op when built with -race.
func (b *Bridge) Close(_ string) {}

// CloseAll is a no-op when built with -race.
func (b *Bridge) CloseAll() {}
