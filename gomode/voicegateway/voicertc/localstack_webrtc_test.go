// Unit tests for local WebRTC voice sessions with placeholder local-stack adapters.

package voicertc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/maruel/gopus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
)

func TestVoiceRTCLocalStackPlaceholders(t *testing.T) {
	t.Parallel()

	t.Run("ASR", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		t.Cleanup(cancel)
		s := newVoiceRTCTestSession(ctx, t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			fixedASR{text: "smoke transcript"}, smokeLLM{text: "ack"}, placeholderTTS{},
		))
		writeLocalMicAudio(ctx, t, s.micTrack)
		data := waitForVoiceRTCMessage(ctx, t, s.messages, s.signalErrs, voicev1.MessageKindTranscriptDelta)
		var msg voicev1.TranscriptDelta
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Speaker != voicev1.SpeakerUser || msg.Text != "smoke transcript" {
			t.Fatalf("transcript = %+v, want user smoke transcript", msg)
		}
	})

	t.Run("LLM", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		t.Cleanup(cancel)
		s := newVoiceRTCTestSession(ctx, t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			fixedASR{text: "hello"}, smokeLLM{text: "smoke llm response"}, placeholderTTS{},
		))
		writeLocalMicAudio(ctx, t, s.micTrack)
		data := waitForVoiceRTCMessage(ctx, t, s.messages, s.signalErrs, voicev1.MessageKindAssistantTextDelta)
		var msg voicev1.AssistantTextDelta
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Text != "smoke llm response" {
			t.Fatalf("assistant text = %q, want smoke llm response", msg.Text)
		}
	})

	t.Run("TTS", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		t.Cleanup(cancel)
		s := newVoiceRTCTestSession(ctx, t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			fixedASR{text: "hello"}, smokeLLM{text: "smoke tts response"}, placeholderTTS{},
		))
		writeLocalMicAudio(ctx, t, s.micTrack)
		waitForVoiceRTCMessage(ctx, t, s.messages, s.signalErrs, voicev1.MessageKindSpeechStarted)
		select {
		case energy := <-s.remoteAudioEnergy:
			if energy < 1_000 {
				t.Fatalf("assistant RTP audio energy = %.0f, want audible signal", energy)
			}
		case err := <-s.mediaErrs:
			t.Fatal(err)
		case err := <-s.signalErrs:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})
}

type voiceRTCTestSession struct {
	micTrack          *webrtc.TrackLocalStaticSample
	messages          <-chan voiceRTCMessage
	signalErrs        <-chan error
	mediaErrs         <-chan error
	remoteAudioEnergy <-chan float64
}

type voiceRTCMessage struct {
	kind voicev1.MessageKind
	data []byte
}

func newVoiceRTCTestSession(ctx context.Context, t *testing.T, backend backendConnector) *voiceRTCTestSession {
	bridge, err := newBridgeWithBackend(ctx, backend, 0)
	if err != nil {
		t.Fatal(err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pc.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(bridge.CloseAll)

	remoteAudioEnergy := make(chan float64, 1)
	mediaErrs := make(chan error, 1)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go collectDecodedAudioEnergy(track, remoteAudioEnergy, mediaErrs)
	})

	micTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio",
		"local-mic",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(micTrack); err != nil {
		t.Fatal(err)
	}

	messages := make(chan voiceRTCMessage, 16)
	signalErrs := make(chan error, 1)
	dcOpen := make(chan struct{})
	dc, err := pc.CreateDataChannel("voice-gateway", nil)
	if err != nil {
		t.Fatal(err)
	}
	dc.OnOpen(func() {
		close(dcOpen)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var env voicev1.MessageEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			select {
			case signalErrs <- err:
			default:
			}
			return
		}
		select {
		case messages <- voiceRTCMessage{kind: env.Kind, data: append([]byte(nil), msg.Data...)}:
		default:
			select {
			case signalErrs <- fmt.Errorf("voice message channel full after %s", env.Kind):
			default:
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gatherDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		t.Fatal("missing local description")
	}
	answerSDP, sessionID, err := bridge.HandleOffer(ctx, localDesc.SDP)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" {
		t.Fatal("empty gateway session ID")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatal(err)
	}

	waitForVoiceRTCOpen(ctx, t, dcOpen, signalErrs)
	sendVoiceRTCJSON(ctx, t, dc, voicev1.SessionSetup{
		Kind:  voicev1.MessageKindSessionSetup,
		Voice: voicev1.VoiceConfig{Name: "local", Language: "en"},
	})
	waitForVoiceRTCMessage(ctx, t, messages, signalErrs, voicev1.MessageKindSessionReady)
	return &voiceRTCTestSession{
		micTrack:          micTrack,
		messages:          messages,
		signalErrs:        signalErrs,
		mediaErrs:         mediaErrs,
		remoteAudioEnergy: remoteAudioEnergy,
	}
}

func collectDecodedAudioEnergy(track *webrtc.TrackRemote, out chan<- float64, errs chan<- error) {
	dec, err := gopus.NewDecoder(encoderSampleRate, 1)
	if err != nil {
		select {
		case errs <- err:
		default:
		}
		return
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		pcm, err := decode48(dec, pkt.Payload)
		if err != nil {
			select {
			case errs <- err:
			default:
			}
			continue
		}
		energy := pcmEnergy(pcm)
		if energy > 1_000 {
			select {
			case out <- energy:
			default:
			}
			return
		}
	}
}

func waitForVoiceRTCOpen(ctx context.Context, t *testing.T, dcOpen <-chan struct{}, errs <-chan error) {
	select {
	case <-dcOpen:
	case err := <-errs:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func sendVoiceRTCJSON(ctx context.Context, t *testing.T, dc *webrtc.DataChannel, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- dc.SendText(string(data))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitForVoiceRTCMessage(
	ctx context.Context,
	t *testing.T,
	messages <-chan voiceRTCMessage,
	errs <-chan error,
	want voicev1.MessageKind,
) []byte {
	for {
		select {
		case msg := <-messages:
			if msg.kind == voicev1.MessageKindError {
				t.Fatal("gateway returned error")
			}
			if msg.kind == want {
				return msg.data
			}
		case err := <-errs:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatalf("%v while waiting for %s", ctx.Err(), want)
		}
	}
}

type smokeLLM struct {
	text string
}

func (l smokeLLM) newConversation(string, []voicev1.ToolDeclaration) llmConversation {
	return smokeLLMConversation(l)
}

type smokeLLMConversation struct {
	text string
}

func (c smokeLLMConversation) user(context.Context, string) (llmStep, error) {
	return newLLMStep([]string{c.text}, llmReply{text: c.text}, nil), nil
}

func (c smokeLLMConversation) toolResult(context.Context, string, string, json.RawMessage) (llmStep, error) {
	return newLLMStep([]string{c.text}, llmReply{text: c.text}, nil), nil
}

func (c smokeLLMConversation) addContext(string) {}

func writeLocalMicAudio(ctx context.Context, t *testing.T, track *webrtc.TrackLocalStaticSample) {
	enc, err := newEncoder()
	if err != nil {
		t.Fatal(err)
	}
	sample := 0
	for frame := range 24 {
		loud := frame < 10
		pcm := make([]int16, encoderFrameSamples)
		if loud {
			for i := range pcm {
				pcm[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(sample)/float64(encoderSampleRate)))
				sample++
			}
		}
		pkt, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if err := track.WriteSample(media.Sample{Data: pkt, Duration: frameDuration}); err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(frameDuration)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		}
	}
}

func pcmEnergy(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var energy float64
	for _, sample := range pcm {
		energy += float64(sample) * float64(sample)
	}
	return energy / float64(len(pcm))
}
