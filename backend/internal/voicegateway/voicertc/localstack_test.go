// Tests for the local stack backend: VAD, turn flow, tool round trip, barge-in.

package voicertc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/base"
	"github.com/maruel/genai/scoreboard"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func TestEnergyVAD(t *testing.T) {
	t.Parallel()

	t.Run("speech then silence yields an utterance", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		utt, active := v.push(loudPCM(200))
		if utt != nil {
			t.Fatalf("utterance during speech = %d bytes, want nil", len(utt))
		}
		if !active {
			t.Fatal("speechActive = false during speech, want true")
		}
		utt, active = v.push(silencePCM(vadSilenceHangoverMS + vadFrameMS))
		if utt == nil {
			t.Fatal("utterance after silence = nil, want non-empty")
		}
		if active {
			t.Fatal("speechActive = true after utterance end, want false")
		}
	})

	t.Run("too-short speech is discarded", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		v.push(loudPCM(vadFrameMS * 2)) // 40ms < vadMinSpeechMS
		utt, _ := v.push(silencePCM(vadSilenceHangoverMS + vadFrameMS))
		if utt != nil {
			t.Fatalf("utterance = %d bytes, want nil for too-short speech", len(utt))
		}
	})

	t.Run("leading silence is dropped", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		utt, active := v.push(silencePCM(200))
		if utt != nil || active {
			t.Fatalf("silence produced utterance=%v active=%v, want nil/false", utt != nil, active)
		}
	})
}

func TestLocalStackTurn(t *testing.T) {
	t.Parallel()
	backend := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, placeholderLLM{}, placeholderTTS{},
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "turn", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	setup := mustJSON(t, voicev1.SessionSetup{
		Kind:  voicev1.MessageKindSessionSetup,
		Voice: voicev1.VoiceConfig{Name: "local", Language: "en"},
		Tools: []voicev1.ToolDeclaration{{Name: "tasks_list", Description: "List tasks", Parameters: json.RawMessage(`{}`)}},
	})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}
	if !sink.isReady() {
		t.Fatal("backend not ready after session.setup")
	}

	// Synthetic utterance: loud speech followed by silence.
	mustAcceptMic(t, sess, loudPCM(200))
	mustAcceptMic(t, sess, silencePCM(vadSilenceHangoverMS+vadFrameMS))

	// The placeholder LLM calls the first declared tool first.
	waitForKind(t, sink, voicev1.MessageKindToolCall)
	if !slices.Contains(sink.kinds(), voicev1.MessageKindTranscriptDelta) {
		t.Fatal("missing user transcript before tool call")
	}
	call := decodeToolCall(t, sink)
	if call.Name != "tasks_list" {
		t.Fatalf("tool call name = %q, want tasks_list", call.Name)
	}

	// Return the tool result; the turn should then speak.
	result := mustJSON(t, voicev1.ToolResult{
		Kind: voicev1.MessageKindToolResult, ID: call.ID, Name: call.Name, Result: json.RawMessage(`{"tasks":[]}`),
	})
	if err := sess.acceptClientMessage(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	waitForKind(t, sink, voicev1.MessageKindSpeechEnded)
	kinds := sink.kinds()
	for _, want := range []voicev1.MessageKind{
		voicev1.MessageKindSpeechStarted,
		voicev1.MessageKindAssistantTextDelta,
		voicev1.MessageKindSpeechEnded,
	} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("kinds = %v, missing %s", kinds, want)
		}
	}
	if sink.pcmLen() == 0 {
		t.Fatal("no assistant audio produced")
	}
}

func TestLocalStackBargeIn(t *testing.T) {
	t.Parallel()
	backend := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		fixedASR{text: "interrupt test"}, echoLLM{}, longTTS{ms: 800},
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "barge", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	// No tools: the echo LLM speaks immediately.
	setup := mustJSON(t, voicev1.SessionSetup{Kind: voicev1.MessageKindSessionSetup})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}

	mustAcceptMic(t, sess, loudPCM(200))
	mustAcceptMic(t, sess, silencePCM(vadSilenceHangoverMS+vadFrameMS))

	// Wait until the assistant is speaking, then barge in with new speech.
	waitForKind(t, sink, voicev1.MessageKindSpeechStarted)
	mustAcceptMic(t, sess, loudPCM(60))

	waitForKind(t, sink, voicev1.MessageKindInterrupted)
	// The interrupted turn must not complete (no speech.ended afterward).
	time.Sleep(50 * time.Millisecond)
	kinds := sink.kinds()
	if slices.Contains(kinds, voicev1.MessageKindSpeechEnded) {
		t.Fatalf("kinds = %v, barge-in must cancel before speech.ended", kinds)
	}
	if sink.wasCanceled() {
		t.Fatal("barge-in must not cancel the whole session")
	}
}

func TestGenaiToolDefs(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tools, err := genaiToolDefs([]voicev1.ToolDeclaration{{
			Name:        "tasks_list",
			Description: "List tasks",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}}}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 1 {
			t.Fatalf("tools = %d, want 1", len(tools))
		}
		if tools[0].Name != "tasks_list" {
			t.Errorf("Name = %q, want tasks_list", tools[0].Name)
		}
		if tools[0].InputSchemaOverride == nil || tools[0].InputSchemaOverride.Type != "object" {
			t.Errorf("InputSchemaOverride = %#v, want object schema", tools[0].InputSchemaOverride)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := genaiToolDefs([]voicev1.ToolDeclaration{{
			Name:        "bad name",
			Description: "Bad",
			Parameters:  json.RawMessage(`{"type":`),
		}})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestGenaiConversation(t *testing.T) {
	t.Parallel()
	p := &fakeGenAIProvider{}
	conv := (&genaiLLMAdapter{provider: p}).newConversation("Answer briefly.", []voicev1.ToolDeclaration{{
		Name:        "tasks_list",
		Description: "List tasks",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}})
	conv.addContext("Project: caic")

	reply, err := conv.user(t.Context(), "What is next?")
	if err != nil {
		t.Fatal(err)
	}
	if reply.toolCall == nil {
		t.Fatal("toolCall = nil, want tool call")
	}
	if reply.toolCall.id == "" {
		t.Fatal("toolCall.id is empty")
	}
	if reply.toolCall.name != "tasks_list" {
		t.Errorf("toolCall.name = %q, want tasks_list", reply.toolCall.name)
	}
	if string(reply.toolCall.args) != `{"limit":1}` {
		t.Errorf("toolCall.args = %s, want limit argument", reply.toolCall.args)
	}
	callID := reply.toolCall.id

	reply, err = conv.toolResult(t.Context(), reply.toolCall.id, reply.toolCall.name, json.RawMessage(`{"tasks":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if reply.text != "Done." {
		t.Errorf("reply.text = %q, want Done.", reply.text)
	}

	calls := p.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if got := calls[0].messages[0].String(); got != "Current context:\nProject: caic\n\nUser said:\nWhat is next?" {
		t.Errorf("first user message = %q", got)
	}
	if len(calls[1].messages) != 3 {
		t.Fatalf("second call messages = %d, want user, assistant tool call, tool result", len(calls[1].messages))
	}
	if got := calls[1].messages[1].Replies[0].ToolCall.ID; got != callID {
		t.Errorf("assistant history tool call ID = %q, want %q", got, callID)
	}
	if len(calls[1].messages[2].ToolCallResults) != 1 {
		t.Fatalf("second call last message = %#v, want tool result", calls[1].messages[2])
	}
	if calls[1].messages[2].ToolCallResults[0].Result != `{"tasks":[]}` {
		t.Errorf("tool result = %q, want JSON object", calls[1].messages[2].ToolCallResults[0].Result)
	}
	if !calls[0].hasSystemPrompt("Answer briefly.") {
		t.Fatal("missing system prompt option")
	}
	if !calls[0].hasTools() {
		t.Fatal("missing tools option")
	}
}

func TestLocalStackModelsForConfig(t *testing.T) {
	t.Parallel()
	var startedModels []string
	var servers []*fakeManagedLlamaServer
	models, err := localStackModelsForConfigWithStarter(t.Context(), &voicegateway.LocalStackConfig{}, func(_ context.Context, model string) (managedLlamaServer, error) {
		startedModels = append(startedModels, model)
		srv := &fakeManagedLlamaServer{url: "http://127.0.0.1:12345"}
		servers = append(servers, srv)
		return srv, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := models.asr.(*genaiASRAdapter); !ok {
		t.Fatalf("asr = %T, want genaiASRAdapter", models.asr)
	}
	if _, ok := models.llm.(*genaiLLMAdapter); !ok {
		t.Fatalf("llm = %T, want genaiLLMAdapter", models.llm)
	}
	wantModels := []string{defaultLocalStackASRModel, defaultLocalStackLLMModel}
	if !slices.Equal(startedModels, wantModels) {
		t.Errorf("started models = %v, want %v", startedModels, wantModels)
	}
	if models.runtime == nil {
		t.Fatal("runtime = nil, want managed server closer")
	}
	if err := models.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	for i, srv := range servers {
		if !srv.closed {
			t.Errorf("managed server %d (%s) was not closed", i, startedModels[i])
		}
	}
}

func TestLocalStackModelsForConfigRequiresProviderWithRemote(t *testing.T) {
	t.Parallel()
	start := func(context.Context, string) (managedLlamaServer, error) {
		return &fakeManagedLlamaServer{url: "http://127.0.0.1:12345"}, nil
	}

	t.Run("asr", func(t *testing.T) {
		t.Parallel()
		cfg := &voicegateway.LocalStackConfig{ASR: voicegateway.LocalStackASRConfig{Remote: "http://127.0.0.1:12345"}}
		_, err := localStackModelsForConfigWithStarter(t.Context(), cfg, start)
		if err == nil || !strings.Contains(err.Error(), "local_stack.asr.provider") {
			t.Fatalf("err = %v, want local_stack.asr.provider error", err)
		}
	})

	t.Run("llm", func(t *testing.T) {
		t.Parallel()
		cfg := &voicegateway.LocalStackConfig{LLM: voicegateway.LocalStackLLMConfig{Remote: "http://127.0.0.1:12345"}}
		_, err := localStackModelsForConfigWithStarter(t.Context(), cfg, start)
		if err == nil || !strings.Contains(err.Error(), "local_stack.llm.provider") {
			t.Fatalf("err = %v, want local_stack.llm.provider error", err)
		}
	})
}

func TestGenaiASRAdapter(t *testing.T) {
	t.Parallel()
	p := &fakeASRProvider{}
	text, err := (&genaiASRAdapter{provider: p}).transcribe(t.Context(), []byte{1, 0, 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want hello world", text)
	}
	if p.mimeType != "audio/wav" {
		t.Errorf("mimeType = %q, want audio/wav", p.mimeType)
	}
	if string(p.wav[:4]) != "RIFF" || string(p.wav[8:12]) != "WAVE" {
		t.Fatalf("wav header = %q/%q, want RIFF/WAVE", p.wav[:4], p.wav[8:12])
	}
	if got := binary.LittleEndian.Uint32(p.wav[24:]); got != micSampleRate {
		t.Errorf("wav sample rate = %d, want %d", got, micSampleRate)
	}
	if got := binary.LittleEndian.Uint32(p.wav[40:]); got != 4 {
		t.Errorf("wav data size = %d, want 4", got)
	}
}

// --- test doubles and helpers ---

type captureSink struct {
	mu       sync.Mutex
	ready    bool
	msgs     [][]byte
	pcm      []byte
	canceled bool
}

func (c *captureSink) backendReady(context.Context) {
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
}

func (c *captureSink) sendGatewayMessage(_ context.Context, data []byte) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, slices.Clone(data))
	c.mu.Unlock()
	return nil
}

func (c *captureSink) sendGatewayError(string) {}

func (c *captureSink) cancelSession() {
	c.mu.Lock()
	c.canceled = true
	c.mu.Unlock()
}

func (c *captureSink) addAssistantPCM(pcm []byte) {
	c.mu.Lock()
	c.pcm = append(c.pcm, pcm...)
	c.mu.Unlock()
}

func (c *captureSink) clearAssistantAudio() {
	c.mu.Lock()
	c.pcm = nil
	c.mu.Unlock()
}

func (c *captureSink) isReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

func (c *captureSink) wasCanceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

func (c *captureSink) pcmLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pcm)
}

func (c *captureSink) kinds() []voicev1.MessageKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	kinds := make([]voicev1.MessageKind, 0, len(c.msgs))
	for _, m := range c.msgs {
		var env voicev1.MessageEnvelope
		if json.Unmarshal(m, &env) == nil {
			kinds = append(kinds, env.Kind)
		}
	}
	return kinds
}

func decodeToolCall(t *testing.T, sink *captureSink) voicev1.ToolCall {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, m := range sink.msgs {
		var env voicev1.MessageEnvelope
		if json.Unmarshal(m, &env) != nil || env.Kind != voicev1.MessageKindToolCall {
			continue
		}
		var call voicev1.ToolCall
		if err := json.Unmarshal(m, &call); err != nil {
			t.Fatal(err)
		}
		return call
	}
	t.Fatal("no tool.call captured")
	return voicev1.ToolCall{}
}

func waitForKind(t *testing.T, sink *captureSink, kind voicev1.MessageKind) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(sink.kinds(), kind) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %v", kind, sink.kinds())
}

func mustAcceptMic(t *testing.T, sess backendSession, pcm []byte) {
	t.Helper()
	if err := sess.acceptMicPCM(t.Context(), pcm); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loudPCM(ms int) []byte {
	samples := micSampleRate * ms / 1000
	b := make([]byte, samples*2)
	for i := range samples {
		v := int16(8000 * math.Sin(2*math.Pi*300*float64(i)/float64(micSampleRate)))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v)) //nolint:gosec // PCM int16→uint16 reinterpret
	}
	return b
}

func silencePCM(ms int) []byte {
	return make([]byte, micSampleRate*2*ms/1000)
}

type echoLLM struct{}

func (echoLLM) newConversation(string, []voicev1.ToolDeclaration) llmConversation { return echoConv{} }

type echoConv struct{}

func (echoConv) user(_ context.Context, text string) (llmReply, error) {
	return llmReply{text: "echo: " + text}, nil
}

func (echoConv) toolResult(context.Context, string, string, json.RawMessage) (llmReply, error) {
	return llmReply{text: "done"}, nil
}

func (echoConv) addContext(string) {}

type longTTS struct{ ms int }

func (t longTTS) synthesize(context.Context, string) ([]byte, error) {
	return make([]byte, backendOutputSampleRate*2*t.ms/1000), nil
}

type fixedASR struct{ text string }

func (a fixedASR) transcribe(context.Context, []byte) (string, error) {
	return a.text, nil
}

type fakeGenAICall struct {
	messages genai.Messages
	opts     []genai.GenOption
}

func (c *fakeGenAICall) hasSystemPrompt(prompt string) bool {
	for _, opt := range c.opts {
		v, ok := opt.(*genai.GenOptionText)
		if ok && v.SystemPrompt == prompt {
			return true
		}
	}
	return false
}

func (c *fakeGenAICall) hasTools() bool {
	for _, opt := range c.opts {
		v, ok := opt.(*genai.GenOptionTools)
		if ok && len(v.Tools) == 1 && v.Tools[0].Name == "tasks_list" {
			return true
		}
	}
	return false
}

type fakeGenAIProvider struct {
	base.NotImplemented

	mu    sync.Mutex
	calls []fakeGenAICall
}

func (p *fakeGenAIProvider) Name() string { return "fake" }

func (p *fakeGenAIProvider) ModelID() string { return "fake-model" }

func (p *fakeGenAIProvider) OutputModalities() genai.Modalities {
	return genai.Modalities{scoreboard.ModalityText}
}

func (p *fakeGenAIProvider) Scoreboard() scoreboard.Score { return scoreboard.Score{} }

func (p *fakeGenAIProvider) HTTPClient() *http.Client { return nil }

func (p *fakeGenAIProvider) GenSync(_ context.Context, msgs genai.Messages, opts ...genai.GenOption) (genai.Result, error) {
	p.mu.Lock()
	p.calls = append(p.calls, fakeGenAICall{
		messages: append(genai.Messages(nil), msgs...),
		opts:     append([]genai.GenOption(nil), opts...),
	})
	callCount := len(p.calls)
	p.mu.Unlock()

	if callCount == 1 {
		return genai.Result{Message: genai.Message{Replies: []genai.Reply{{
			ToolCall: genai.ToolCall{Name: "tasks_list", Arguments: `{"limit":1}`},
		}}}}, nil
	}
	return genai.Result{Message: genai.Message{Replies: []genai.Reply{{Text: "Done."}}}}, nil
}

func (p *fakeGenAIProvider) callsSnapshot() []fakeGenAICall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]fakeGenAICall(nil), p.calls...)
}

type fakeManagedLlamaServer struct {
	url    string
	closed bool
}

func (s *fakeManagedLlamaServer) URL() string { return s.url }

func (s *fakeManagedLlamaServer) Close() error {
	s.closed = true
	return nil
}

type fakeASRProvider struct {
	base.NotImplemented

	mimeType string
	wav      []byte
}

func (p *fakeASRProvider) Name() string { return "fake-asr" }

func (p *fakeASRProvider) ModelID() string { return "fake-model" }

func (p *fakeASRProvider) OutputModalities() genai.Modalities {
	return genai.Modalities{scoreboard.ModalityText}
}

func (p *fakeASRProvider) Scoreboard() scoreboard.Score { return scoreboard.Score{} }

func (p *fakeASRProvider) HTTPClient() *http.Client { return nil }

func (p *fakeASRProvider) GenSync(_ context.Context, msgs genai.Messages, _ ...genai.GenOption) (genai.Result, error) {
	doc := msgs[0].Requests[1].Doc
	mimeType, data, err := doc.Read(10 * 1024 * 1024)
	if err != nil {
		return genai.Result{}, err
	}
	p.mimeType = mimeType
	p.wav = data
	return genai.Result{Message: genai.Message{Replies: []genai.Reply{{Text: "hello world"}}}}, nil
}
