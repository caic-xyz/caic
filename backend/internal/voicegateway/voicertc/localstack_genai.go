// Local stack model adapters for local ASR, LLM, and TTS runtimes.

package voicertc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/maruel/genai/providers/llamacpp"
	"github.com/maruel/genai/providers/llamacpp/llamacppsrv"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func localStackBackendForConfig(ctx context.Context, cfg *voicegateway.LocalStackConfig) (*localStackBackend, error) {
	models, err := localStackModelsForConfigWithStarter(ctx, cfg, startManagedLlamaServer)
	if err != nil {
		return nil, err
	}
	tts, err := newKittenTTSAdapter(ctx)
	if err != nil {
		if models.runtime != nil {
			_ = models.runtime.Close()
		}
		return nil, err
	}
	b := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		models.asr,
		models.llm,
		tts,
	)
	b.runtime = joinClosers(models.runtime, tts)
	return b, nil
}

type localStackModels struct {
	asr     asrAdapter
	llm     llmAdapter
	runtime io.Closer
}

// localStackEndpoint is a resolved genai provider plus the managed runtime
// that backs it, if the gateway started one (nil for remote/registry providers
// it does not own).
type localStackEndpoint struct {
	provider genai.Provider
	runtime  io.Closer
}

func localStackModelsForConfigWithStarter(
	ctx context.Context,
	cfg *voicegateway.LocalStackConfig,
	start managedLlamaStarter,
) (localStackModels, error) {
	asr, err := resolveLocalStackEndpoint(ctx, "local_stack.asr", cfg.ASR.Provider, cfg.ASR.Remote, cfg.ASR.Model, defaultLocalStackASRModel, start)
	if err != nil {
		return localStackModels{}, err
	}
	llm, err := resolveLocalStackEndpoint(ctx, "local_stack.llm", cfg.LLM.Provider, cfg.LLM.Remote, cfg.LLM.Model, defaultLocalStackLLMModel, start)
	if err != nil {
		if asr.runtime != nil {
			_ = asr.runtime.Close()
		}
		return localStackModels{}, err
	}
	return localStackModels{
		asr:     &genaiASRAdapter{provider: asr.provider},
		llm:     &genaiLLMAdapter{provider: llm.provider},
		runtime: joinClosers(asr.runtime, llm.runtime),
	}, nil
}

// resolveLocalStackEndpoint wires one local stack adapter (ASR or LLM) to its
// configured provider: a managed (or remote) llama.cpp runtime by default, or
// any provider registered in providers.All when provider names one explicitly.
// prefix names the config table in error messages, e.g. "local_stack.asr".
func resolveLocalStackEndpoint(ctx context.Context, prefix, provider, remote, model, defaultModel string, start managedLlamaStarter) (localStackEndpoint, error) {
	p := provider
	if p == "" {
		if remote != "" {
			return localStackEndpoint{}, fmt.Errorf("%s.provider is required when %s.remote is set", prefix, prefix)
		}
		p = "llamacpp"
	}
	if p == "llamacpp" {
		return localStackLlamaEndpoint(ctx, remote, model, defaultModel, start)
	}
	return localStackGenAIEndpoint(ctx, p, remote, model)
}

// localStackLlamaEndpoint wires the managed (or remote) llama.cpp runtime. A
// managed server was just started by us, so it is known to be reachable; a
// user-supplied remote is pinged to fail fast on misconfiguration.
func localStackLlamaEndpoint(ctx context.Context, remote, model, defaultModel string, start managedLlamaStarter) (localStackEndpoint, error) {
	var runtime io.Closer
	if remote == "" {
		wantModel := model
		if wantModel == "" {
			wantModel = defaultModel
		}
		srv, err := start(ctx, wantModel)
		if err != nil {
			return localStackEndpoint{}, err
		}
		remote = srv.URL()
		runtime = srv
		model = "" // the managed server is already pinned to wantModel
	}
	p, err := newLlamaProvider(ctx, remote, model)
	if err != nil {
		if runtime != nil {
			_ = runtime.Close()
		}
		return localStackEndpoint{}, err
	}
	if runtime == nil {
		if err := pingLocalStackProvider(ctx, p); err != nil {
			return localStackEndpoint{}, err
		}
	}
	return localStackEndpoint{provider: p, runtime: runtime}, nil
}

// localStackGenAIEndpoint wires any provider registered in providers.All (e.g.
// ollama, openaicompatible pointed at a local server). We don't manage the
// provider's process, so ping it at startup to fail fast if it's unreachable.
func localStackGenAIEndpoint(ctx context.Context, provider, remote, model string) (localStackEndpoint, error) {
	entry, ok := providers.All[provider]
	if !ok || entry.Factory == nil {
		return localStackEndpoint{}, fmt.Errorf("unsupported local stack provider %q", provider)
	}
	var opts []genai.ProviderOption
	if remote != "" {
		opts = append(opts, genai.ProviderOptionRemote(remote))
	}
	if model != "" {
		opts = append(opts, genai.ProviderOptionModel(model))
	}
	p, err := entry.Factory(ctx, opts...)
	if err != nil {
		return localStackEndpoint{}, fmt.Errorf("create %s provider: %w", provider, err)
	}
	if err := pingLocalStackProvider(ctx, p); err != nil {
		return localStackEndpoint{}, err
	}
	return localStackEndpoint{provider: p}, nil
}

// joinClosers combines closers into one that closes all of them, joining any
// errors. Nil entries are skipped.
func joinClosers(closers ...io.Closer) io.Closer {
	var live []io.Closer
	for _, c := range closers {
		if c != nil {
			live = append(live, c)
		}
	}
	return multiCloser(live)
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	errs := make([]error, len(m))
	for i, c := range m {
		errs[i] = c.Close()
	}
	return errors.Join(errs...)
}

func pingLocalStackProvider(ctx context.Context, p genai.Provider) error {
	pinger, ok := p.(genai.ProviderPing)
	if !ok {
		return nil
	}
	if err := pinger.Ping(ctx); err != nil {
		return fmt.Errorf("ping %s provider: %w", p.Name(), err)
	}
	return nil
}

type managedLlamaServer interface {
	io.Closer
	URL() string
}

type managedLlamaStarter func(context.Context, string) (managedLlamaServer, error)

func startManagedLlamaServer(ctx context.Context, model string) (managedLlamaServer, error) {
	build := llamacppsrv.BuildNumber
	cache, err := localStackLlamaCacheDir(build)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return nil, fmt.Errorf("create llama.cpp cache dir: %w", err)
	}
	exe, err := llamacppsrv.DownloadRelease(ctx, cache, build)
	if err != nil {
		return nil, err
	}
	hostPort, err := localStackLlamaHostPort(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"-hf", model, "--no-warmup"}
	slog.InfoContext(ctx, "voicertc: starting managed llama.cpp", "model", model, "build", build, "hostPort", hostPort)
	logger := slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo)
	srv, err := llamacppsrv.New(ctx, exe, "", logger.Writer(), hostPort, 0, args)
	if err != nil {
		return nil, fmt.Errorf("start managed llama.cpp: %w", err)
	}
	slog.InfoContext(ctx, "voicertc: managed llama.cpp ready", "url", srv.URL())
	return srv, nil
}

func localStackLlamaHostPort(ctx context.Context) (string, error) {
	const host = "127.0.0.1"
	var cfg net.ListenConfig
	l, err := cfg.Listen(ctx, "tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("select llama.cpp port: %w", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		return "", fmt.Errorf("selected llama.cpp listener has address %T, want *net.TCPAddr", l.Addr())
	}
	if err := l.Close(); err != nil {
		return "", fmt.Errorf("release llama.cpp port probe: %w", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(addr.Port)), nil
}

func localStackLlamaCacheDir(build int) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("get user cache dir: %w", err)
	}
	return filepath.Join(base, "caic", "llama-server", strconv.Itoa(build)), nil
}

const (
	// defaultLocalStackASRModel is a small dedicated speech-to-text model: it
	// transcribes far more cheaply and reliably than asking the conversational
	// LLM to read inline audio.
	defaultLocalStackASRModel = "ggml-org/Qwen3-ASR-0.6B-GGUF:Q8_0"
	// defaultLocalStackLLMModel handles conversation state and tool calls.
	defaultLocalStackLLMModel = "unsloth/gemma-4-E2B-it-GGUF:UD-Q4_K_XL"
)

func newLlamaProvider(ctx context.Context, remote, model string) (genai.Provider, error) {
	opts := []genai.ProviderOption{genai.ProviderOptionRemote(remote)}
	if model != "" {
		opts = append(opts, genai.ProviderOptionModel(model))
	}
	p, err := llamacpp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create llama.cpp client: %w", err)
	}
	return p, nil
}

type genaiASRAdapter struct {
	provider genai.Provider
}

func (a *genaiASRAdapter) transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	wav := pcmS16LEMonoWAV(pcm, micSampleRate)
	msg := genai.Message{Requests: []genai.Request{
		{Text: "Transcribe the attached audio. Return only the transcript text, with no commentary."},
		{Doc: genai.Doc{Filename: "speech.wav", Src: bytes.NewReader(wav)}},
	}}
	res, err := a.provider.GenSync(ctx, genai.Messages{msg},
		&genai.GenOptionText{SystemPrompt: "You are a speech recognition engine. Return only the spoken words."},
		&llamacpp.GenOption{},
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.String()), nil
}

type genaiLLMAdapter struct {
	provider genai.Provider
}

func (a *genaiLLMAdapter) newConversation(systemInstruction string, tools []voicev1.ToolDeclaration) llmConversation {
	defs, err := genaiToolDefs(tools)
	return &genaiConversation{
		provider:          a.provider,
		systemInstruction: systemInstruction,
		tools:             defs,
		initErr:           err,
	}
}

type genaiConversation struct {
	provider genai.Provider

	mu                sync.Mutex
	messages          genai.Messages
	systemInstruction string
	contextText       string
	tools             []genai.ToolDef
	initErr           error
	nextToolID        int
}

func (c *genaiConversation) user(ctx context.Context, text string) (llmReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initErr != nil {
		return llmReply{}, c.initErr
	}
	c.messages = append(c.messages, genai.NewTextMessage(c.userText(text)))
	return c.generateLocked(ctx)
}

func (c *genaiConversation) toolResult(ctx context.Context, id, name string, result json.RawMessage) (llmReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initErr != nil {
		return llmReply{}, c.initErr
	}
	resultText := string(result)
	if resultText == "" {
		resultText = "null"
	}
	c.messages = append(c.messages, genai.Message{ToolCallResults: []genai.ToolCallResult{{
		ID:     id,
		Name:   name,
		Result: resultText,
	}}})
	return c.generateLocked(ctx)
}

func (c *genaiConversation) addContext(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.contextText == "" {
		c.contextText = text
		return
	}
	c.contextText += "\n\n" + text
}

func (c *genaiConversation) generateLocked(ctx context.Context) (llmReply, error) {
	opts := []genai.GenOption{
		&genai.GenOptionText{SystemPrompt: c.systemInstruction},
		&llamacpp.GenOption{},
	}
	if len(c.tools) != 0 {
		opts = append(opts, &genai.GenOptionTools{Tools: c.tools})
	}
	res, err := c.provider.GenSync(ctx, c.messages, opts...)
	if err != nil {
		return llmReply{}, err
	}
	reply := c.toReplyLocked(&res.Message)
	c.messages = append(c.messages, res.Message)
	return reply, nil
}

func (c *genaiConversation) toReplyLocked(msg *genai.Message) llmReply {
	text := strings.Builder{}
	for i := range msg.Replies {
		reply := &msg.Replies[i]
		if !reply.ToolCall.IsZero() {
			if reply.ToolCall.ID == "" {
				c.nextToolID++
				reply.ToolCall.ID = fmt.Sprintf("local-call-%d", c.nextToolID)
			}
			if reply.ToolCall.Arguments == "" {
				reply.ToolCall.Arguments = "{}"
			}
			return llmReply{toolCall: &llmToolCall{
				id:   reply.ToolCall.ID,
				name: reply.ToolCall.Name,
				args: json.RawMessage(reply.ToolCall.Arguments),
			}}
		}
		text.WriteString(reply.Text)
	}
	return llmReply{text: text.String()}
}

func (c *genaiConversation) userText(text string) string {
	if c.contextText == "" {
		return text
	}
	return "Current context:\n" + c.contextText + "\n\nUser said:\n" + text
}

func genaiToolDefs(tools []voicev1.ToolDeclaration) ([]genai.ToolDef, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]genai.ToolDef, len(tools))
	var errs []error
	for i := range tools {
		schema := genai.JSONSchema(`{"type":"object"}`)
		if len(tools[i].Parameters) != 0 {
			var schemaObject map[string]json.RawMessage
			if err := json.Unmarshal(tools[i].Parameters, &schemaObject); err != nil {
				errs = append(errs, fmt.Errorf("tool %q parameters: %w", tools[i].Name, err))
			} else {
				schema = append(genai.JSONSchema(nil), tools[i].Parameters...)
			}
		}
		out[i] = genai.ToolDef{
			Name:                tools[i].Name,
			Description:         tools[i].Description,
			InputSchemaOverride: schema,
		}
		if err := out[i].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("tool %q: %w", tools[i].Name, err))
		}
	}
	return out, errors.Join(errs...)
}

func pcmS16LEMonoWAV(pcm []byte, sampleRate int) []byte {
	const (
		headerSize    = 44
		audioFormat   = 1
		channelCount  = 1
		bitsPerSample = 16
	)
	out := make([]byte, headerSize+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm))) //nolint:gosec // WAV files are bounded by captured utterance size.
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], audioFormat)
	binary.LittleEndian.PutUint16(out[22:], channelCount)
	binary.LittleEndian.PutUint32(out[24:], uint32(sampleRate)) //nolint:gosec // Sample rate is a small constant.
	byteRate := sampleRate * channelCount * bitsPerSample / 8
	binary.LittleEndian.PutUint32(out[28:], uint32(byteRate)) //nolint:gosec // Byte rate is derived from a small sample rate.
	blockAlign := channelCount * bitsPerSample / 8
	binary.LittleEndian.PutUint16(out[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:], bitsPerSample)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm))) //nolint:gosec // WAV files are bounded by captured utterance size.
	copy(out[44:], pcm)
	return out
}
