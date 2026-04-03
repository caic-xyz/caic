// Tests for opus CGo bindings. Requires libopus-dev.

//go:build cgo && !windows

package opus

import (
	"math"
	"testing"
)

func TestOpus(t *testing.T) {
	const sampleRate = 16000

	t.Run("RoundTrip", func(t *testing.T) {
		enc, err := NewEncoder(sampleRate, 1, AppVoIP)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(sampleRate, 1)
		if err != nil {
			t.Fatal(err)
		}

		// Generate a 20ms 440Hz sine tone.
		const frameSamples = sampleRate * 20 / 1000
		pcm := make([]int16, frameSamples)
		for i := range pcm {
			pcm[i] = int16(math.Sin(2*math.Pi*440*float64(i)/sampleRate) * 16000)
		}

		encoded := make([]byte, 4000)
		n, err := enc.Encode(pcm, encoded)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("encoded zero bytes")
		}
		encoded = encoded[:n]

		decoded := make([]int16, frameSamples)
		samples, err := dec.Decode(encoded, decoded)
		if err != nil {
			t.Fatal(err)
		}
		if samples != frameSamples {
			t.Fatalf("decoded %d samples, want %d", samples, frameSamples)
		}
	})

	t.Run("Stereo", func(t *testing.T) {
		enc, err := NewEncoder(sampleRate, 2, AppVoIP)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(sampleRate, 2)
		if err != nil {
			t.Fatal(err)
		}

		const frameSamples = sampleRate * 20 / 1000
		pcm := make([]int16, frameSamples*2) // interleaved stereo
		for i := range pcm {
			pcm[i] = int16(math.Sin(2*math.Pi*440*float64(i/2)/sampleRate) * 16000)
		}

		encoded := make([]byte, 4000)
		n, err := enc.Encode(pcm, encoded)
		if err != nil {
			t.Fatal(err)
		}

		decoded := make([]int16, frameSamples*2)
		samples, err := dec.Decode(encoded[:n], decoded)
		if err != nil {
			t.Fatal(err)
		}
		if samples != frameSamples {
			t.Fatalf("decoded %d samples, want %d", samples, frameSamples)
		}
	})

	t.Run("InvalidChannels", func(t *testing.T) {
		_, err := NewEncoder(sampleRate, 3, AppVoIP)
		if err == nil {
			t.Fatal("expected error for 3 channels")
		}
		_, err = NewDecoder(sampleRate, 0)
		if err == nil {
			t.Fatal("expected error for 0 channels")
		}
	})

	t.Run("EncodeEmptyInput", func(t *testing.T) {
		enc, err := NewEncoder(sampleRate, 1, AppVoIP)
		if err != nil {
			t.Fatal(err)
		}
		_, err = enc.Encode(nil, make([]byte, 4000))
		if err == nil {
			t.Fatal("expected error for empty PCM")
		}
		_, err = enc.Encode(make([]int16, 320), nil)
		if err == nil {
			t.Fatal("expected error for empty target")
		}
	})

	t.Run("DecodeEmptyInput", func(t *testing.T) {
		dec, err := NewDecoder(sampleRate, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = dec.Decode(nil, make([]int16, 320))
		if err == nil {
			t.Fatal("expected error for empty data")
		}
		_, err = dec.Decode([]byte{0xff}, nil)
		if err == nil {
			t.Fatal("expected error for empty target")
		}
	})
}
