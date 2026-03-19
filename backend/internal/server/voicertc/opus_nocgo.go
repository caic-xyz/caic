// Stub codec when CGo is disabled. WebRTC bridge falls back to data-channel-only passthrough.

//go:build !cgo

package voicertc

import "errors"

const codecAvailable = false

var errNoCodec = errors.New("opus codec unavailable: build with CGO_ENABLED=1 and libopus-dev")

type opusDecoder struct{}

func newDecoder() (*opusDecoder, error) { return nil, errNoCodec }

func (d *opusDecoder) Decode(_ []byte) ([]int16, error) { return nil, errNoCodec }

type opusEncoder struct{}

func newEncoder() (*opusEncoder, error) { return nil, errNoCodec }

func (e *opusEncoder) Encode(_ []int16) ([]byte, error) { return nil, errNoCodec }
