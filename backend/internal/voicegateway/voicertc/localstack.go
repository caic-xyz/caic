// Local stack backend adapter: VAD, ASR, LLM, and TTS for half-duplex voice.

package voicertc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"sync"
	"time"

	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

// Local stack tuning. These bound the placeholder VAD/segmentation and TTS
// pacing; real model adapters arrive in a later phase.
const (
	vadFrameMS           = 20
	vadFrameBytes        = micSampleRate * 2 * vadFrameMS / 1000 // 640 bytes @16kHz mono
	vadRMSThreshold      = 500.0
	vadSilenceHangoverMS = 200
	vadMinSpeechMS       = 100
	// ttsChunkBytes is one 20ms frame of backend PCM output, fed to the sink.
	ttsChunkBytes = backendOutputFrameBytes
)

// vadSegmenter splits a mic PCM stream into complete utterances. push consumes a
// chunk of S16LE mono PCM at micSampleRate and returns a completed utterance's
// PCM when end-of-speech is detected (nil otherwise) plus whether speech is
// currently active (used for barge-in detection).
type vadSegmenter interface {
	push(pcm []byte) (utterance []byte, speechActive bool)
	reset()
}

// asrAdapter converts an utterance's PCM into final text.
type asrAdapter interface {
	transcribe(ctx context.Context, pcm []byte) (string, error)
}

// llmToolCall is a normalized tool invocation requested by the local LLM.
type llmToolCall struct {
	id   string
	name string
	args json.RawMessage
}

// llmReply is one step of an LLM turn: assistant text or a tool call.
type llmReply struct {
	text     string
	toolCall *llmToolCall
}

// llmConversation is a stateful conversation with the local LLM.
type llmConversation interface {
	user(ctx context.Context, text string) (llmReply, error)
	toolResult(ctx context.Context, id, name string, result json.RawMessage) (llmReply, error)
	addContext(text string)
}

// llmAdapter builds conversations bound to a session's system instruction and tools.
type llmAdapter interface {
	newConversation(systemInstruction string, tools []voicev1.ToolDeclaration) llmConversation
}

// ttsAdapter streams S16LE mono PCM at backendOutputSampleRate from text.
type ttsAdapter interface {
	synthesize(ctx context.Context, text string) iter.Seq2[[]byte, error]
}

// localStackBackend is a backendConnector that runs a half-duplex
// VAD→ASR→LLM→TTS pipeline. The adapters are pluggable; the default ones are
// deterministic placeholders until real local models are wired in.
type localStackBackend struct {
	newVAD  func() vadSegmenter
	asr     asrAdapter
	llm     llmAdapter
	tts     ttsAdapter
	runtime io.Closer
}

func newLocalStackBackend(newVAD func() vadSegmenter, asr asrAdapter, llm llmAdapter, tts ttsAdapter) *localStackBackend {
	return &localStackBackend{newVAD: newVAD, asr: asr, llm: llm, tts: tts}
}

func (b *localStackBackend) Close() error {
	if b.runtime == nil {
		return nil
	}
	return b.runtime.Close()
}

func (b *localStackBackend) connect(ctx context.Context, sessionID string, sink backendSink) (backendSession, error) {
	return &localStackSession{
		id:          sessionID,
		sink:        sink,
		baseCtx:     ctx,
		vad:         b.newVAD(),
		asr:         b.asr,
		llm:         b.llm,
		tts:         b.tts,
		toolResults: make(chan toolResultMsg, 1),
	}, nil
}

// toolResultMsg carries a client tool result back to a waiting turn.
type toolResultMsg struct {
	id     string
	name   string
	result json.RawMessage
}

// localStackSession owns one half-duplex voice session: queues, cancellation,
// and tool-call correlation are all local to the backend.
type localStackSession struct {
	id      string
	sink    backendSink
	baseCtx context.Context //nolint:containedctx // session lifetime context for turn goroutines

	vad vadSegmenter
	asr asrAdapter
	llm llmAdapter
	tts ttsAdapter

	mu             sync.Mutex
	conv           llmConversation
	turnCancel     context.CancelFunc
	turnGeneration int
	speaking       bool
	toolResults    chan toolResultMsg
}

func (s *localStackSession) acceptClientMessage(ctx context.Context, data []byte) error {
	var env voicev1.MessageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode gateway message: %w", err)
	}
	switch env.Kind {
	case voicev1.MessageKindSessionSetup:
		var msg voicev1.SessionSetup
		if err := json.Unmarshal(data, &msg); err != nil {
			return fmt.Errorf("decode session.setup: %w", err)
		}
		s.mu.Lock()
		s.conv = s.llm.newConversation(msg.Context.SystemInstruction, msg.Tools)
		s.mu.Unlock()
		s.sink.backendReady(ctx)
		return nil
	case voicev1.MessageKindContextUpdate:
		var msg voicev1.ContextUpdate
		if err := json.Unmarshal(data, &msg); err != nil {
			return fmt.Errorf("decode context.update: %w", err)
		}
		s.mu.Lock()
		conv := s.conv
		s.mu.Unlock()
		if conv != nil && msg.Context.Text != "" {
			conv.addContext(msg.Context.Text)
		}
		return nil
	case voicev1.MessageKindUserMessage:
		var msg voicev1.UserMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return fmt.Errorf("decode user.message: %w", err)
		}
		s.startUserMessage(ctx, msg.Text)
		return nil
	case voicev1.MessageKindToolResult:
		var msg voicev1.ToolResult
		if err := json.Unmarshal(data, &msg); err != nil {
			return fmt.Errorf("decode tool.result: %w", err)
		}
		select {
		case s.toolResults <- toolResultMsg{id: msg.ID, name: msg.Name, result: msg.Result}:
		default:
		}
		return nil
	case voicev1.MessageKindTurnCancel:
		s.bargeIn(voicev1.InterruptSourceUser, "turn cancelled")
		return nil
	case voicev1.MessageKindSessionClose:
		return errSessionClosed
	default:
		return fmt.Errorf("unsupported gateway message kind %q", env.Kind)
	}
}

func (s *localStackSession) acceptMicPCM(ctx context.Context, pcm []byte) error {
	utterance, speechActive := s.vad.push(pcm)
	if speechActive && s.isSpeaking() {
		// New user speech while the assistant is talking is a barge-in.
		s.bargeIn(voicev1.InterruptSourceUser, "")
	}
	if utterance != nil {
		s.startTurn(ctx, utterance)
	}
	return nil
}

func (s *localStackSession) close() error {
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
	}
	s.mu.Unlock()
	return nil
}

func (s *localStackSession) clearTurnCancel(generation int) {
	s.mu.Lock()
	if s.turnGeneration == generation {
		s.turnCancel = nil
	}
	s.mu.Unlock()
}

func (s *localStackSession) startUserMessage(ctx context.Context, text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	turnCtx, cancel := context.WithCancel(ctx)
	s.turnCancel = cancel
	s.turnGeneration++
	generation := s.turnGeneration
	conv := s.conv
	s.mu.Unlock()
	if conv == nil {
		cancel()
		return
	}
	go func() {
		defer s.clearTurnCancel(generation)
		reply, err := conv.user(turnCtx, text)
		if err != nil {
			s.warnTurn("llm", err)
			return
		}
		s.handleReply(turnCtx, conv, reply)
	}()
}

func (s *localStackSession) startTurn(ctx context.Context, utterance []byte) {
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	turnCtx, cancel := context.WithCancel(ctx)
	s.turnCancel = cancel
	s.turnGeneration++
	generation := s.turnGeneration
	conv := s.conv
	s.mu.Unlock()
	if conv == nil {
		cancel()
		return
	}
	// Discard any stale tool result left from a previous turn.
	select {
	case <-s.toolResults:
	default:
	}
	go func() {
		s.runTurn(turnCtx, conv, utterance)
		s.clearTurnCancel(generation)
	}()
}

func (s *localStackSession) runTurn(ctx context.Context, conv llmConversation, utterance []byte) {
	text, err := s.asr.transcribe(ctx, utterance)
	if err != nil {
		s.warnTurn("asr", err)
		return
	}
	if text == "" {
		return
	}
	s.emit(&voicev1.TranscriptDelta{Kind: voicev1.MessageKindTranscriptDelta, Speaker: voicev1.SpeakerUser, Text: text})
	reply, err := conv.user(ctx, text)
	if err != nil {
		s.warnTurn("llm", err)
		return
	}
	s.handleReply(ctx, conv, reply)
}

func (s *localStackSession) handleReply(ctx context.Context, conv llmConversation, reply llmReply) {
	for {
		if ctx.Err() != nil {
			return
		}
		if reply.toolCall != nil {
			s.emit(&voicev1.ToolCall{
				Kind: voicev1.MessageKindToolCall,
				ID:   reply.toolCall.id,
				Name: reply.toolCall.name,
				Args: reply.toolCall.args,
			})
			res, ok := s.waitToolResult(ctx)
			if !ok {
				return
			}
			next, err := conv.toolResult(ctx, res.id, res.name, res.result)
			if err != nil {
				s.warnTurn("llm", err)
				return
			}
			reply = next
			continue
		}
		s.speak(ctx, reply.text)
		return
	}
}

func (s *localStackSession) speak(ctx context.Context, text string) {
	if text == "" {
		return
	}
	var started bool
	var audioBytes int
	defer func() {
		if started {
			s.setSpeaking(false)
		}
	}()

	for pcm, err := range s.tts.synthesize(ctx, text) {
		if err != nil {
			if ctx.Err() == nil {
				s.warnTurn("tts", err)
				if started {
					s.emit(&voicev1.SpeechEnded{Kind: voicev1.MessageKindSpeechEnded, Speaker: voicev1.SpeakerAssistant})
				}
			}
			return
		}
		if len(pcm) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if !started {
			started = true
			s.setSpeaking(true)
			s.emit(&voicev1.SpeechStarted{Kind: voicev1.MessageKindSpeechStarted, Speaker: voicev1.SpeakerAssistant})
			s.emit(&voicev1.TranscriptDelta{Kind: voicev1.MessageKindTranscriptDelta, Speaker: voicev1.SpeakerAssistant, Text: text})
			s.emit(&voicev1.AssistantTextDelta{Kind: voicev1.MessageKindAssistantTextDelta, Text: text})
		}
		for off := 0; off < len(pcm); off += ttsChunkBytes {
			if ctx.Err() != nil {
				return
			}
			end := min(off+ttsChunkBytes, len(pcm))
			s.sink.addAssistantPCM(pcm[off:end])
			audioBytes += end - off
		}
	}
	if !started {
		return
	}
	// Hold the speaking state for the audio's duration so barge-in remains
	// meaningful while the bridge drains the buffer at realtime.
	audioDur := time.Duration(audioBytes/2) * time.Second / time.Duration(backendOutputSampleRate)
	if !sleepCtx(ctx, audioDur) {
		return
	}
	s.emit(&voicev1.SpeechEnded{Kind: voicev1.MessageKindSpeechEnded, Speaker: voicev1.SpeakerAssistant})
}

func (s *localStackSession) bargeIn(source voicev1.InterruptSource, message string) {
	s.mu.Lock()
	cancel := s.turnCancel
	s.turnCancel = nil
	s.speaking = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.sink.clearAssistantAudio()
	select {
	case <-s.toolResults:
	default:
	}
	s.emit(&voicev1.Interrupted{Kind: voicev1.MessageKindInterrupted, Source: source, Message: message})
}

func (s *localStackSession) waitToolResult(ctx context.Context) (toolResultMsg, bool) {
	select {
	case r := <-s.toolResults:
		return r, true
	case <-ctx.Done():
		return toolResultMsg{}, false
	}
}

func (s *localStackSession) emit(msg any) {
	if err := s.sink.sendGatewayMessage(s.baseCtx, mustGatewayServerMessage(msg)); err != nil {
		s.sink.cancelSession()
	}
}

func (s *localStackSession) setSpeaking(v bool) {
	s.mu.Lock()
	s.speaking = v
	s.mu.Unlock()
}

func (s *localStackSession) isSpeaking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speaking
}

func (s *localStackSession) warnTurn(stage string, err error) {
	slog.Warn("voicertc: local stack turn failed", "session", s.id, "stage", stage, "err", err)
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// energyVAD is a simple RMS-energy voice activity detector with a silence
// hangover. It is a placeholder until a real VAD/segmentation model is wired in.
type energyVAD struct {
	leftover  []byte
	utterance []byte
	speaking  bool
	speechMS  int
	silenceMS int
}

func (v *energyVAD) push(pcm []byte) ([]byte, bool) {
	v.leftover = append(v.leftover, pcm...)
	for len(v.leftover) >= vadFrameBytes {
		frame := v.leftover[:vadFrameBytes]
		v.leftover = v.leftover[vadFrameBytes:]
		if frameRMS(frame) > vadRMSThreshold {
			v.speaking = true
			v.speechMS += vadFrameMS
			v.silenceMS = 0
			v.utterance = append(v.utterance, frame...)
			continue
		}
		if !v.speaking {
			continue // drop leading silence
		}
		v.silenceMS += vadFrameMS
		v.utterance = append(v.utterance, frame...)
		if v.silenceMS >= vadSilenceHangoverMS {
			done := v.utterance
			hadSpeech := v.speechMS >= vadMinSpeechMS
			v.resetUtterance()
			if hadSpeech {
				return done, false
			}
		}
	}
	return nil, v.speaking
}

func (v *energyVAD) reset() {
	v.leftover = nil
	v.resetUtterance()
}

func (v *energyVAD) resetUtterance() {
	v.utterance = nil
	v.speaking = false
	v.speechMS = 0
	v.silenceMS = 0
}

func frameRMS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := range n {
		sample := int16(binary.LittleEndian.Uint16(frame[i*2:])) //nolint:gosec // PCM uint16→int16 reinterpret
		sum += float64(sample) * float64(sample)
	}
	return math.Sqrt(sum / float64(n))
}

// placeholderASR returns a fixed transcript. Real ASR is wired in a later phase.
type placeholderASR struct{}

func (placeholderASR) transcribe(_ context.Context, _ []byte) (string, error) {
	return "(local speech input)", nil
}

// placeholderLLM calls the first declared tool once, then answers with text.
type placeholderLLM struct{}

func (placeholderLLM) newConversation(_ string, tools []voicev1.ToolDeclaration) llmConversation {
	return &placeholderConversation{tools: tools}
}

type placeholderConversation struct {
	tools      []voicev1.ToolDeclaration
	calledTool bool
}

func (c *placeholderConversation) user(_ context.Context, text string) (llmReply, error) {
	if len(c.tools) > 0 && !c.calledTool {
		c.calledTool = true
		return llmReply{toolCall: &llmToolCall{
			id:   "local-call-1",
			name: c.tools[0].Name,
			args: json.RawMessage(`{}`),
		}}, nil
	}
	return llmReply{text: "You said: " + text}, nil
}

func (c *placeholderConversation) toolResult(_ context.Context, _, _ string, _ json.RawMessage) (llmReply, error) {
	return llmReply{text: "Done."}, nil
}

func (c *placeholderConversation) addContext(_ string) {}

// placeholderTTS emits a quiet tone whose length scales with the text. Real TTS
// is wired in a later phase.
type placeholderTTS struct{}

func (placeholderTTS) synthesize(_ context.Context, text string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if text == "" {
			return
		}
		const (
			msPerChar = 50
			maxMS     = 4000
			toneHz    = 220.0
			amplitude = 3000.0
		)
		durationMS := max(min(len(text)*msPerChar, maxMS), vadFrameMS)
		samples := backendOutputSampleRate * durationMS / 1000
		pcm := make([]byte, samples*2)
		for i := range samples {
			v := int16(amplitude * math.Sin(2*math.Pi*toneHz*float64(i)/float64(backendOutputSampleRate)))
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v)) //nolint:gosec // PCM int16→uint16 reinterpret
		}
		yield(pcm, nil)
	}
}
