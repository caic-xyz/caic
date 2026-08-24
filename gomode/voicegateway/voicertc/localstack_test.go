// Tests for the local stack backend: VAD, turn flow, tool round trip, barge-in.

package voicertc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"iter"
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

	"github.com/caic-xyz/caic/gomode/voicegateway"
	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
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

func TestLocalStackUserMessage(t *testing.T) {
	t.Parallel()
	backend := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, placeholderLLM{}, placeholderTTS{},
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "say", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	setup := mustJSON(t, voicev1.SessionSetup{Kind: voicev1.MessageKindSessionSetup})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}
	msg := mustJSON(t, voicev1.UserMessage{Kind: voicev1.MessageKindUserMessage, Text: "Say exactly one word: Ready"})
	if err := sess.acceptClientMessage(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	waitForKind(t, sink, voicev1.MessageKindSpeechEnded)
	if !slices.Contains(sink.kinds(), voicev1.MessageKindSpeechStarted) {
		t.Fatal("missing speech.started")
	}
	wantText := "You said: Say exactly one word: Ready"
	if text := sink.assistantText(); text != wantText {
		t.Fatalf("assistant text = %q, want %q", text, wantText)
	}
	if sink.pcmLen() == 0 {
		t.Fatal("no assistant audio produced")
	}
}

func TestLocalStackToolInitialTurnSpeaksStreamedTextBeforeToolCall(t *testing.T) {
	t.Parallel()
	conv := &fakeConversation{
		userStep: fakeLLMStep{
			deltas: []string{"Let me check. "},
			reply: llmReply{toolCall: &llmToolCall{
				id:   "call-1",
				name: "tasks_list",
				args: json.RawMessage(`{}`),
			}},
		},
	}
	tts := &recordingTTS{}
	backend := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, fixedConversationLLM{conv: conv}, tts,
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "tool-stream", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	setup := mustJSON(t, voicev1.SessionSetup{
		Kind:  voicev1.MessageKindSessionSetup,
		Tools: []voicev1.ToolDeclaration{{Name: "tasks_list", Parameters: json.RawMessage(`{}`)}},
	})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}
	msg := mustJSON(t, voicev1.UserMessage{Kind: voicev1.MessageKindUserMessage, Text: "List tasks"})
	if err := sess.acceptClientMessage(t.Context(), msg); err != nil {
		t.Fatal(err)
	}

	waitForKind(t, sink, voicev1.MessageKindToolCall)
	if conv.userCalls() != 1 {
		t.Fatalf("user calls = %d, want 1", conv.userCalls())
	}
	if got, want := tts.textsSnapshot(), []string{"Let me check. "}; !slices.Equal(got, want) {
		t.Fatalf("tts texts before tool result = %#v, want %#v", got, want)
	}
	if got := sink.assistantText(); got != "Let me check. " {
		t.Fatalf("assistant text before tool result = %q, want streamed pre-tool text", got)
	}
}

func TestLocalStackChainsToolCallsWithStreamedText(t *testing.T) {
	t.Parallel()
	conv := &fakeConversation{
		userStep: fakeLLMStep{
			deltas: []string{"Looking. "},
			reply: llmReply{toolCall: &llmToolCall{
				id:   "call-1",
				name: "tasks_list",
				args: json.RawMessage(`{}`),
			}},
		},
		toolResultSteps: []fakeLLMStep{
			{
				deltas: []string{"Checking details. "},
				reply:  llmReply{toolCall: &llmToolCall{id: "call-2", name: "tasks_get", args: json.RawMessage(`{}`)}},
			},
			{deltas: []string{"Done. Next"}, reply: llmReply{text: "Done. Next"}},
		},
	}
	tts := &recordingTTS{}
	backend := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, fixedConversationLLM{conv: conv}, tts,
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "tool-chain", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	setup := mustJSON(t, voicev1.SessionSetup{
		Kind: voicev1.MessageKindSessionSetup,
		Tools: []voicev1.ToolDeclaration{
			{Name: "tasks_list", Parameters: json.RawMessage(`{}`)},
			{Name: "tasks_get", Parameters: json.RawMessage(`{}`)},
		},
	})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}
	msg := mustJSON(t, voicev1.UserMessage{Kind: voicev1.MessageKindUserMessage, Text: "List tasks"})
	if err := sess.acceptClientMessage(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	waitForKindCount(t, sink, voicev1.MessageKindToolCall, 1)
	result := mustJSON(t, voicev1.ToolResult{
		Kind: voicev1.MessageKindToolResult, ID: "call-1", Name: "tasks_list", Result: json.RawMessage(`{"tasks":[]}`),
	})
	if err := sess.acceptClientMessage(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	waitForKindCount(t, sink, voicev1.MessageKindToolCall, 2)

	result = mustJSON(t, voicev1.ToolResult{
		Kind: voicev1.MessageKindToolResult, ID: "call-2", Name: "tasks_get", Result: json.RawMessage(`{"task":null}`),
	})
	if err := sess.acceptClientMessage(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	waitForKind(t, sink, voicev1.MessageKindSpeechEnded)
	if conv.toolResultCalls() != 2 {
		t.Fatalf("tool result calls = %d, want 2", conv.toolResultCalls())
	}
	if got, want := tts.textsSnapshot(), []string{"Looking. ", "Checking details. ", "Done. ", "Next"}; !slices.Equal(got, want) {
		t.Fatalf("tts texts = %#v, want %#v", got, want)
	}
	if got := sink.assistantText(); got != "Looking. Checking details. Done. Next" {
		t.Fatalf("assistant text = %q, want streamed chained text", got)
	}
}

func TestLocalStackSessionSpeak(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		firstChunk := make(chan struct{})
		releaseSecondChunk := make(chan struct{})
		sink := &captureSink{}
		s := &localStackSession{
			id:      "tts-stream",
			sink:    sink,
			baseCtx: t.Context(),
			tts:     blockingTTS{firstChunk: firstChunk, releaseSecondChunk: releaseSecondChunk},
		}
		done := make(chan struct{})
		go func() {
			s.speak(t.Context(), "hello")
			close(done)
		}()
		select {
		case <-firstChunk:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for first streamed TTS chunk")
		}
		if got := sink.pcmLen(); got != 2 {
			t.Fatalf("pcmLen = %d, want first chunk before synthesis completes", got)
		}
		close(releaseSecondChunk)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for speak to finish")
		}
		if got := sink.pcmLen(); got != 4 {
			t.Fatalf("pcmLen = %d, want both streamed chunks", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		ttsCalled := make(chan struct{})
		sink := &captureSink{}
		s := &localStackSession{
			id:      "tts-error",
			sink:    sink,
			baseCtx: t.Context(),
			tts:     failingTTS{called: ttsCalled},
		}
		s.speak(t.Context(), "hello")
		select {
		case <-ttsCalled:
		default:
			t.Fatal("TTS was not called")
		}
		if kinds := sink.kinds(); len(kinds) != 0 {
			t.Fatalf("kinds = %v, want no speech events after TTS failure", kinds)
		}
		if sink.pcmLen() != 0 {
			t.Fatal("assistant audio produced after TTS failure")
		}
		if sink.wasCanceled() {
			t.Fatal("TTS failure must not cancel the whole session")
		}
	})
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
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(tools[0].InputSchemaOverride, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Type != "object" {
			t.Errorf("InputSchemaOverride type = %q, want object", schema.Type)
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

	step, err := conv.user(t.Context(), "What is next?")
	if err != nil {
		t.Fatal(err)
	}
	reply, deltas := finishLLMStep(t, step)
	if len(deltas) != 0 {
		t.Fatalf("initial text deltas = %#v, want none before tool call", deltas)
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

	step, err = conv.toolResult(t.Context(), reply.toolCall.id, reply.toolCall.name, json.RawMessage(`{"tasks":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	reply, deltas = finishLLMStep(t, step)
	if want := []string{"Done."}; !slices.Equal(deltas, want) {
		t.Fatalf("post-tool text deltas = %#v, want %#v", deltas, want)
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
	if !calls[1].hasTools() {
		t.Fatal("post-tool generation omitted tools")
	}
}

func finishLLMStep(t *testing.T, step llmStep) (reply llmReply, deltas []string) {
	deltas = slices.Collect(step.text)
	reply, err := step.finish()
	if err != nil {
		t.Fatal(err)
	}
	return reply, deltas
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

func (c *captureSink) assistantText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	for _, m := range c.msgs {
		var env voicev1.MessageEnvelope
		if json.Unmarshal(m, &env) != nil || env.Kind != voicev1.MessageKindAssistantTextDelta {
			continue
		}
		var msg voicev1.AssistantTextDelta
		if json.Unmarshal(m, &msg) == nil {
			out.WriteString(msg.Text)
		}
	}
	return out.String()
}

func decodeToolCall(t *testing.T, sink *captureSink) voicev1.ToolCall {
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
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(sink.kinds(), kind) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %v", kind, sink.kinds())
}

func waitForKindCount(t *testing.T, sink *captureSink, kind voicev1.MessageKind, count int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := kindCount(sink.kinds(), kind); got >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s messages; got %v", count, kind, sink.kinds())
}

func kindCount(kinds []voicev1.MessageKind, want voicev1.MessageKind) int {
	count := 0
	for _, kind := range kinds {
		if kind == want {
			count++
		}
	}
	return count
}

func mustAcceptMic(t *testing.T, sess backendSession, pcm []byte) {
	if err := sess.acceptMicPCM(t.Context(), pcm); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
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

func (echoConv) user(_ context.Context, text string) (llmStep, error) {
	return newLLMStep([]string{"echo: " + text}, llmReply{text: "echo: " + text}, nil), nil
}

func (echoConv) toolResult(context.Context, string, string, json.RawMessage) (llmStep, error) {
	return newLLMStep([]string{"done"}, llmReply{text: "done"}, nil), nil
}

func (echoConv) addContext(string) {}

type longTTS struct{ ms int }

func (t longTTS) synthesize(context.Context, string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		yield(make([]byte, backendOutputSampleRate*2*t.ms/1000), nil)
	}
}

type failingTTS struct {
	called chan struct{}
}

func (t failingTTS) synthesize(context.Context, string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		close(t.called)
		yield(nil, errors.New("synthesis failed"))
	}
}

type blockingTTS struct {
	firstChunk         chan struct{}
	releaseSecondChunk chan struct{}
}

func (t blockingTTS) synthesize(context.Context, string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if !yield([]byte{1, 2}, nil) {
			return
		}
		close(t.firstChunk)
		<-t.releaseSecondChunk
		yield([]byte{3, 4}, nil)
	}
}

type recordingTTS struct {
	mu    sync.Mutex
	texts []string
}

func (t *recordingTTS) synthesize(_ context.Context, text string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		t.mu.Lock()
		t.texts = append(t.texts, text)
		t.mu.Unlock()
		yield([]byte{1, 2}, nil)
	}
}

func (t *recordingTTS) textsSnapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.texts...)
}

type fixedConversationLLM struct{ conv llmConversation }

func (l fixedConversationLLM) newConversation(string, []voicev1.ToolDeclaration) llmConversation {
	return l.conv
}

type fakeLLMStep struct {
	deltas []string
	reply  llmReply
	err    error
}

type fakeConversation struct {
	mu sync.Mutex

	userStep        fakeLLMStep
	toolResultStep  fakeLLMStep
	toolResultSteps []fakeLLMStep

	userCount       int
	toolResultCount int
}

func (c *fakeConversation) user(context.Context, string) (llmStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userCount++
	return newLLMStep(c.userStep.deltas, c.userStep.reply, c.userStep.err), nil
}

func (c *fakeConversation) toolResult(context.Context, string, string, json.RawMessage) (llmStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolResultCount++
	if c.toolResultCount <= len(c.toolResultSteps) {
		step := c.toolResultSteps[c.toolResultCount-1]
		return newLLMStep(step.deltas, step.reply, step.err), nil
	}
	return newLLMStep(c.toolResultStep.deltas, c.toolResultStep.reply, c.toolResultStep.err), nil
}

func (c *fakeConversation) addContext(string) {}

func (c *fakeConversation) userCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userCount
}

func (c *fakeConversation) toolResultCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toolResultCount
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

func (p *fakeGenAIProvider) GenStream(_ context.Context, msgs genai.Messages, opts ...genai.GenOption) (fragmentsSeq iter.Seq[genai.Reply], finish func() (genai.Result, error)) {
	p.mu.Lock()
	p.calls = append(p.calls, fakeGenAICall{
		messages: append(genai.Messages(nil), msgs...),
		opts:     append([]genai.GenOption(nil), opts...),
	})
	callCount := len(p.calls)
	p.mu.Unlock()

	var replies []genai.Reply
	if callCount == 1 {
		replies = []genai.Reply{{ToolCall: genai.ToolCall{Name: "tasks_list", Arguments: `{"limit":1}`}}}
	} else {
		replies = []genai.Reply{{Text: "Done."}}
	}
	fragments := append([]genai.Reply(nil), replies...)
	return func(yield func(genai.Reply) bool) {
			for i := range fragments {
				if !yield(fragments[i]) {
					return
				}
			}
		}, func() (genai.Result, error) {
			return genai.Result{Replies: replies}, nil
		}
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
	return genai.Result{Replies: []genai.Reply{{Text: "hello world"}}}, nil
}
