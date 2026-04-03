// Stub when CGo is disabled or on Windows. All operations return ErrNotAvailable.

//go:build !cgo || windows

package opus

import "errors"

// ErrNotAvailable is returned when the opus codec is not compiled in.
var ErrNotAvailable = errors.New("opus codec unavailable: build with CGO_ENABLED=1 and libopus-dev")

// Application specifies the intended use case for the encoder.
type Application int

const (
	// AppVoIP optimizes encoding for voice over IP.
	AppVoIP Application = 0x2048 // OPUS_APPLICATION_VOIP
)

// Encoder is a stub.
type Encoder struct{}

// NewEncoder returns ErrNotAvailable.
func NewEncoder(int, int, Application) (*Encoder, error) { return nil, ErrNotAvailable }

// Encode returns ErrNotAvailable.
func (enc *Encoder) Encode([]int16, []byte) (int, error) { return 0, ErrNotAvailable }

// Decoder is a stub.
type Decoder struct{}

// NewDecoder returns ErrNotAvailable.
func NewDecoder(int, int) (*Decoder, error) { return nil, ErrNotAvailable }

// Decode returns ErrNotAvailable.
func (dec *Decoder) Decode([]byte, []int16) (int, error) { return 0, ErrNotAvailable }
