// Tests for the WebRTC voice bridge IPv4-only network layer and audio pipeline.

//go:build !race

package voicertc

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/maruel/gopus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestNewBridge(t *testing.T) {
	t.Parallel()
	t.Run("NewBridge", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll()
	})

	t.Run("PeerConnection", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll()

		pc, err := b.api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal("create PeerConnection:", err)
		}
		defer func() { _ = pc.Close() }()

		if _, err := pc.CreateDataChannel("test", nil); err != nil {
			t.Fatal("create data channel:", err)
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal("create offer:", err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			t.Fatal("set local description:", err)
		}
	})
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Parallel()
	const (
		freq       = 440.0
		durationMs = 200
		amplitude  = 28000.0
		samples24  = 24000 * durationMs / 1000
	)

	pcm24Bytes := make([]byte, samples24*2)
	for i := range samples24 {
		ts := float64(i) / 24000.0
		s := math.Sin(2 * math.Pi * freq * ts)
		binary.LittleEndian.PutUint16(pcm24Bytes[i*2:], uint16(int16(s*amplitude))) //nolint:gosec // fits in int16
	}

	// 24kHz → 48kHz upsampling.
	pcm48 := upsample24to48(pcm24Bytes)
	if len(pcm48) != samples24*2 {
		t.Fatalf("upsample: got %d samples, want %d", len(pcm48), samples24*2)
	}

	// Encode at 48kHz, 20ms frames (960 samples).
	enc, err := newEncoder()
	if err != nil {
		t.Fatalf("newEncoder: %v", err)
	}
	var packets [][]byte
	for i := 0; i+encoderFrameSamples <= len(pcm48); i += encoderFrameSamples {
		pkt, encErr := enc.Encode(pcm48[i : i+encoderFrameSamples])
		if encErr != nil {
			t.Fatalf("Encode at %d: %v", i, encErr)
		}
		packets = append(packets, pkt)
	}
	frames := len(pcm48) / encoderFrameSamples
	if len(packets) != frames {
		t.Fatalf("packets: got %d, want %d", len(packets), frames)
	}

	// Decode at 48kHz.
	dec, err := gopus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("gopus.NewDecoder: %v", err)
	}
	var decoded []int16
	for _, pkt := range packets {
		samples, decErr := decode48(dec, pkt)
		if decErr != nil {
			t.Fatalf("Decode: %v", decErr)
		}
		decoded = append(decoded, samples...)
	}

	expectedLen := frames * encoderFrameSamples
	if len(decoded) < expectedLen-1 || len(decoded) > expectedLen+1 {
		t.Fatalf("decoded length: got %d, want ~%d", len(decoded), expectedLen)
	}

	var energy float64
	for _, s := range decoded {
		energy += float64(s) * float64(s)
	}
	energy /= float64(len(decoded))
	if energy < 5e6 {
		t.Errorf("signal too quiet: energy=%.0f", energy)
	}

	crossings := countZeroCrossings(decoded)
	expectedZC := int(freq * float64(durationMs) / 1000 * 2)
	if percentDiff(crossings, expectedZC) > 25 {
		t.Errorf("zero-crossings: got %d, want %d (±25%%)", crossings, expectedZC)
	}

	t.Logf("roundtrip OK: %d frames, %d samples, energy=%.0f, zc=%d",
		frames, len(decoded), energy, crossings)
}

// decode48 decodes an Opus packet at 48kHz into int16 samples.
func decode48(dec *gopus.Decoder, pkt []byte) ([]int16, error) {
	pcm := make([]int16, maxFrameSamples)
	n, err := dec.Decode(pkt, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}

func countZeroCrossings(samples []int16) int {
	if len(samples) < 2 {
		return 0
	}
	n := 0
	for i := 1; i < len(samples); i++ {
		if (samples[i-1] < 0) != (samples[i] < 0) {
			n++
		}
	}
	return n
}

func percentDiff(a, b int) int {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return 100
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d * 100 / b
}

func TestUpsampleEdgeCases(t *testing.T) {
	t.Parallel()
	if g := upsample24to48(nil); len(g) != 0 {
		t.Errorf("nil: got %d", len(g))
	}
	if g := upsample24to48([]byte{}); len(g) != 0 {
		t.Errorf("empty: got %d", len(g))
	}

	single := make([]byte, 2)
	binary.LittleEndian.PutUint16(single, 0x3039) // 12345
	g := upsample24to48(single)
	if len(g) != 2 {
		t.Fatalf("single: got %d, want 2", len(g))
	}
	if g[0] != 12345 || g[1] != 12345 {
		t.Errorf("single: got [%d %d], want [12345 12345]", g[0], g[1])
	}

	two := make([]byte, 4)
	binary.LittleEndian.PutUint16(two[0:], 0)
	binary.LittleEndian.PutUint16(two[2:], 1000)
	g = upsample24to48(two)
	if len(g) != 4 {
		t.Fatalf("two: got %d, want 4", len(g))
	}
	if g[0] != 0 || g[1] != 500 || g[2] != 1000 || g[3] != 1000 {
		t.Errorf("two: got [%d %d %d %d], want [0 500 1000 1000]", g[0], g[1], g[2], g[3])
	}
}

func TestWriteSampleHasBinding(t *testing.T) {
	t.Parallel()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	if len(pc.GetSenders()) != 1 {
		t.Fatalf("expected 1 sender, got %d", len(pc.GetSenders()))
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if len(pc.GetSenders()) != 1 {
		t.Fatal("sender lost after SetLocalDescription")
	}

	if err := track.WriteSample(media.Sample{
		Data:     []byte{0xfc, 0xff, 0xfe},
		Duration: frameDuration,
	}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	t.Log("WriteSample binding OK")
}
