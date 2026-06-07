// Opus codec wrappers using the pure-Go gopus library.

//go:build race

package voicertc

import "errors"

const (
	// maxFrameSamples is the max samples per decoded Opus frame (48kHz, 120ms).
	maxFrameSamples = 48000 * 120 / 1000

	// maxOpusPacketSize is a safe upper bound for an encoded Opus packet.
	maxOpusPacketSize = 4000
)

// opusDecoder wraps gopus for Opus->PCM (16kHz mono output).
type opusDecoder struct{}

func newDecoder() (*opusDecoder, error) {
	return nil, errors.New("opus is disabled in race builds")
}

// Decode decodes an Opus packet into PCM 16kHz mono samples.
func (d *opusDecoder) Decode(pkt []byte) ([]int16, error) {
	return nil, errors.New("opus is disabled in race builds")
}

// opusEncoder wraps gopus for PCM->Opus (48kHz mono, AppVoIP).
type opusEncoder struct{}

func newEncoder() (*opusEncoder, error) {
	return nil, errors.New("opus is disabled in race builds")
}

// Encode encodes PCM 48kHz mono samples into an Opus packet.
func (e *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	return nil, errors.New("opus is disabled in race builds")
}
