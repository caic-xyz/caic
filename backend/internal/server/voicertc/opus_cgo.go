// Opus codec wrappers using libopus via CGo. Built only when CGo is enabled.

//go:build cgo && !windows

package voicertc

import (
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/opus"
)

const codecAvailable = true

const (
	// maxFrameSamples is the max samples per decoded Opus frame (48kHz, 120ms).
	maxFrameSamples = 48000 * 120 / 1000

	// maxOpusPacketSize is a safe upper bound for an encoded Opus packet.
	maxOpusPacketSize = 4000
)

// opusDecoder wraps libopus for Opus->PCM (16kHz mono output).
type opusDecoder struct {
	dec *opus.Decoder
}

func newDecoder() (*opusDecoder, error) {
	dec, err := opus.NewDecoder(sampleRate, 1)
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

// opusEncoder wraps libopus for PCM->Opus (16kHz mono, AppVoIP).
type opusEncoder struct {
	enc *opus.Encoder
}

func newEncoder() (*opusEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	return &opusEncoder{enc: enc}, nil
}

// Encode encodes PCM 16kHz mono samples into an Opus packet.
func (e *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	data := make([]byte, maxOpusPacketSize)
	n, err := e.enc.Encode(pcm, data)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}
