// Opus codec wrappers using the pure-Go gopus library.

package voicertc

import (
	"fmt"

	"github.com/maruel/gopus"
)

const (
	// maxFrameSamples is the max samples per decoded Opus frame (48kHz, 120ms).
	maxFrameSamples = 48000 * 120 / 1000

	// maxOpusPacketSize is a safe upper bound for an encoded Opus packet.
	maxOpusPacketSize = 4000
)

// opusDecoder wraps gopus for Opus->PCM (16kHz mono output).
type opusDecoder struct {
	dec *gopus.Decoder
}

func newDecoder() (*opusDecoder, error) {
	dec, err := gopus.NewDecoder(inputSampleRate, 1)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	return &opusDecoder{dec: dec}, nil
}

// Decode decodes an Opus packet into PCM 16kHz mono samples.
func (d *opusDecoder) Decode(pkt []byte) ([]int16, error) {
	pcm := make([]int16, maxFrameSamples)
	n, err := d.dec.Decode(pkt, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}

// opusEncoder wraps gopus for PCM->Opus (48kHz mono, AppVoIP).
type opusEncoder struct {
	enc *gopus.Encoder
}

func newEncoder() (*opusEncoder, error) {
	enc, err := gopus.NewEncoder(encoderSampleRate, 1, gopus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	return &opusEncoder{enc: enc}, nil
}

// Encode encodes PCM 48kHz mono samples into an Opus packet.
func (e *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	data := make([]byte, maxOpusPacketSize)
	n, err := e.enc.Encode(pcm, data)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}
