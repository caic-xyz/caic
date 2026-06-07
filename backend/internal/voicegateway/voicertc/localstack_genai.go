// Local stack genai adapters for local LLM runtimes.

package voicertc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers/llamacpp"
	"github.com/maruel/genai/providers/llamacpp/llamacppsrv"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func localStackBackendForConfig(ctx context.Context, cfg *voicegateway.LocalStackConfig) (*localStackBackend, error) {
	models, err := localStackModelsForConfigWithStarter(ctx, &cfg.LLM, startManagedLlamaServer)
	if err != nil {
		return nil, err
	}
	b := newLocalStackBackend(
		func() vadSegmenter { return &energyVAD{} },
		models.asr,
		models.llm,
		placeholderTTS{},
	)
	b.closeRuntime = models.close
	return b, nil
}

type localStackModels struct {
	asr   asrAdapter
	llm   llmAdapter
	close func() error
}

func localStackModelsForConfigWithStarter(
	ctx context.Context,
	cfg *voicegateway.LocalStackLLMConfig,
	start managedLlamaStarter,
) (localStackModels, error) {
	switch cfg.Provider {
	case "", "llamacpp":
		remote := cfg.Remote
		var closeRuntime func() error
		if remote == "" {
			srv, err := start(ctx, localStackLlamaModel(cfg))
			if err != nil {
				return localStackModels{}, err
			}
			remote = srv.URL()
			closeRuntime = srv.Close
		}
		model := cfg.Model
		if closeRuntime != nil {
			model = ""
		}
		p, err := newLlamaProvider(ctx, remote, model)
		if err != nil {
			if closeRuntime != nil {
				_ = closeRuntime()
			}
			return localStackModels{}, err
		}
		return localStackModels{
			asr:   &genaiASRAdapter{provider: p},
			llm:   &genaiLLMAdapter{provider: p},
			close: closeRuntime,
		}, nil
	default:
		return localStackModels{}, fmt.Errorf("unsupported local stack llm provider %q", cfg.Provider)
	}
}

type managedLlamaServer interface {
	URL() string
	Close() error
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
	args := []string{"-hf", model}
	slog.InfoContext(ctx, "voicertc: starting managed llama.cpp", "model", model, "build", build, "hostPort", "127.0.0.1:0")
	logger := slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo)
	srv, err := llamacppsrv.New(ctx, exe, "", logger.Writer(), "127.0.0.1:0", 0, args)
	if err != nil {
		return nil, fmt.Errorf("start managed llama.cpp: %w", err)
	}
	slog.InfoContext(ctx, "voicertc: managed llama.cpp ready", "url", srv.URL())
	return srv, nil
}

func localStackLlamaCacheDir(build int) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("get user cache dir: %w", err)
	}
	return filepath.Join(base, "caic", "llama-server", strconv.Itoa(build)), nil
}

func localStackLlamaModel(cfg *voicegateway.LocalStackLLMConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	return "unsloth/gemma-4-E2B-it-GGUF:UD-Q4_K_XL"
}

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
		schema := &jsonschema.Schema{Type: "object"}
		if len(tools[i].Parameters) != 0 {
			if err := json.Unmarshal(tools[i].Parameters, schema); err != nil {
				errs = append(errs, fmt.Errorf("tool %q parameters: %w", tools[i].Name, err))
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
