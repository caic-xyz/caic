// Minimal CGo bindings to libopus for encoding and decoding Opus audio.

//go:build cgo && !windows

package opus

/*
#cgo pkg-config: opus
#include <opus.h>
*/
import "C" //nolint:gocritic // CGo requires separate import block.

import (
	"errors"
	"unsafe" //nolint:gocritic // dupImport false positive: CGo requires separate import "C" block.
)

// Application specifies the intended use case for the encoder.
type Application int

const (
	// AppVoIP optimizes encoding for voice over IP.
	AppVoIP = Application(C.OPUS_APPLICATION_VOIP)
)

// Error represents a libopus error code.
type Error int

func (e Error) Error() string {
	return "opus: " + C.GoString(C.opus_strerror(C.int(e)))
}

// Encoder encodes PCM audio to Opus.
type Encoder struct {
	p        *C.struct_OpusEncoder
	channels int
	mem      []byte // Go-heap allocation so GC manages the lifetime.
}

var (
	errBadChannels = errors.New("opus: channels must be 1 or 2")
	errNoPCM       = errors.New("opus: no PCM data")
	errNoTargetBuf = errors.New("opus: no target buffer")
	errNoData      = errors.New("opus: no data")
)

// NewEncoder creates an Opus encoder for the given sample rate, channel count,
// and application profile.
func NewEncoder(sampleRate, channels int, app Application) (*Encoder, error) {
	if channels != 1 && channels != 2 {
		return nil, errBadChannels
	}
	size := C.opus_encoder_get_size(C.int(channels))
	mem := make([]byte, size)
	p := (*C.OpusEncoder)(unsafe.Pointer(&mem[0]))
	errno := C.opus_encoder_init(p, C.opus_int32(sampleRate), C.int(channels), C.int(app))
	if errno != C.OPUS_OK {
		return nil, Error(errno)
	}
	return &Encoder{p: p, channels: channels, mem: mem}, nil
}

// Encode encodes PCM int16 samples into data, returning bytes written.
func (enc *Encoder) Encode(pcm []int16, data []byte) (int, error) {
	if len(pcm) == 0 {
		return 0, errNoPCM
	}
	if len(data) == 0 {
		return 0, errNoTargetBuf
	}
	samples := len(pcm) / enc.channels
	n := int(C.opus_encode(
		enc.p,
		(*C.opus_int16)(&pcm[0]),
		C.int(samples),
		(*C.uchar)(&data[0]),
		C.opus_int32(cap(data))))
	if n < 0 {
		return 0, Error(n)
	}
	return n, nil
}

// Decoder decodes Opus packets to PCM audio.
type Decoder struct {
	p        *C.struct_OpusDecoder
	channels int
	mem      []byte
}

// NewDecoder creates an Opus decoder for the given sample rate and channel count.
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	if channels != 1 && channels != 2 {
		return nil, errBadChannels
	}
	size := C.opus_decoder_get_size(C.int(channels))
	mem := make([]byte, size)
	p := (*C.OpusDecoder)(unsafe.Pointer(&mem[0]))
	errno := C.opus_decoder_init(p, C.opus_int32(sampleRate), C.int(channels))
	if errno != C.OPUS_OK {
		return nil, Error(errno)
	}
	return &Decoder{p: p, channels: channels, mem: mem}, nil
}

// Decode decodes an Opus packet into PCM int16 samples, returning samples written.
func (dec *Decoder) Decode(data []byte, pcm []int16) (int, error) {
	if len(data) == 0 {
		return 0, errNoData
	}
	if len(pcm) == 0 {
		return 0, errNoTargetBuf
	}
	n := int(C.opus_decode(
		dec.p,
		(*C.uchar)(&data[0]),
		C.opus_int32(len(data)),
		(*C.opus_int16)(&pcm[0]),
		C.int(cap(pcm)/dec.channels),
		0))
	if n < 0 {
		return 0, Error(n)
	}
	return n, nil
}
