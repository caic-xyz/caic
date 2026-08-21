// Command check-agent-logs validates recent v2 task-log harness records against genai wire DTOs.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	caicopencode "github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/klauspost/compress/zstd"
	claudedto "github.com/maruel/genai/providers/claudecode"
	codexdto "github.com/maruel/genai/providers/codex"
	opencodedto "github.com/maruel/genai/providers/opencode"
	pidto "github.com/maruel/genai/providers/pi"
)

const maxRecordSize = 32 << 20

var knownClaudeSystemSubtypes = map[claudedto.SystemSubtype]struct{}{
	claudedto.SystemInit:        {},
	claudedto.SystemTaskStarted: {}, claudedto.SystemTaskNotification: {},
	claudedto.SystemTaskProgress: {}, claudedto.SystemTaskUpdated: {},
	claudedto.SystemBackgroundTasksChanged: {}, claudedto.SystemThinkingTokens: {},
	claudedto.SystemTurnDuration: {}, claudedto.SystemCompactBoundary: {},
	claudedto.SystemStatus: {}, claudedto.SystemCommandsChanged: {},
	claudedto.SystemSessionStateChanged: {}, claudedto.SystemAPIRetry: {},
	claudedto.SystemLocalCommandOutput: {}, claudedto.SystemHookStarted: {},
	claudedto.SystemHookProgress: {}, claudedto.SystemHookResponse: {},
	claudedto.SystemFilesPersisted: {}, claudedto.SystemElicitationComplete: {},
	claudedto.SystemPostTurnSummary: {}, claudedto.SystemModelRefusalFallback: {},
	claudedto.SystemModelRefusalNoFallback: {}, claudedto.SystemVCSStateChanged: {},
}

type config struct {
	dir   string
	since time.Duration
	all   bool
	paths []string
}

type record struct {
	Type    string `json:"t"`
	Harness string `json:"harness"`
	Version int    `json:"version"`
}

type finding struct {
	path    string
	line    int
	harness string
	dto     string
	err     error
	hint    string
}

func (f finding) String() string {
	hint := f.hint
	if hint == "" {
		hint = "update: ~/src/genai/providers/" + providerDir(f.harness) + "/dto.go"
	}
	return fmt.Sprintf("%s:%d: harness=%s dto=%s: %v\n  %s", f.path, f.line, f.harness, f.dto, f.err, hint)
}

func providerDir(harness string) string {
	if harness == "claude" {
		return "claudecode"
	}
	return harness
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	paths, err := logPaths(cfg)
	if err != nil {
		return err
	}
	var findings []finding
	var checked int
	for _, path := range paths {
		fs, isV2, err := checkFile(path)
		if err != nil {
			return err
		}
		if isV2 {
			checked++
		}
		findings = append(findings, fs...)
	}
	findings = dedupe(findings)
	for _, f := range findings {
		if _, err := fmt.Fprintln(out, f.String()); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "checked %d v2 task logs; found %d schema issues\n", checked, len(findings)); err != nil {
		return err
	}
	if len(findings) != 0 {
		return errors.New("agent log schema validation failed")
	}
	return nil
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("check-agent-logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.dir, "dir", defaultLogDir(), "task-log directory")
	fs.DurationVar(&cfg.since, "since", 72*time.Hour, "only scan files modified within this duration")
	fs.BoolVar(&cfg.all, "all", false, "scan all matching files regardless of modification time")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.since < 0 {
		return config{}, errors.New("-since must not be negative")
	}
	cfg.paths = fs.Args()
	return cfg, nil
}

func defaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".cache", "caic", "tasks")
}

func logPaths(cfg config) ([]string, error) {
	var roots []string
	if len(cfg.paths) == 0 {
		roots = []string{cfg.dir}
	} else {
		roots = cfg.paths
	}
	var paths []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", root, err)
		}
		if !info.IsDir() {
			if isLogPath(root) {
				paths = append(paths, root)
			}
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !isLogPath(entry.Name()) {
				continue
			}
			path := filepath.Join(root, entry.Name())
			if len(cfg.paths) == 0 && !cfg.all {
				info, err := entry.Info()
				if err != nil {
					return nil, fmt.Errorf("stat %s: %w", path, err)
				}
				if time.Since(info.ModTime()) > cfg.since {
					continue
				}
			}
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func isLogPath(path string) bool {
	return strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".jsonl.zst")
}

func checkFile(path string) ([]finding, bool, error) {
	r, err := openLog(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = r.Close() }()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), maxRecordSize)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, false, fmt.Errorf("read %s: %w", path, err)
		}
		return nil, false, nil
	}

	var header record
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return nil, false, fmt.Errorf("decode %s:1: %w", path, err)
	}
	if header.Type != "caic_meta" || header.Version != int(agent.LogVersionV2) {
		return nil, false, nil
	}
	if header.Harness == "" {
		return nil, true, fmt.Errorf("validate %s:1: v2 metadata is missing harness", path)
	}

	var findings []finding
	openCodeMethods := make(map[string]opencodedto.Method)
	line := 1
	parser, err := agent.NewLogRecordParser(agent.LogVersionV2, func(message []byte) ([]agent.Message, error) {
		if f := checkMessage(path, line, header.Harness, message, openCodeMethods); f != nil {
			findings = append(findings, *f)
		}
		return nil, nil
	})
	if err != nil {
		return nil, true, fmt.Errorf("create v2 parser: %w", err)
	}
	if _, err := parser.ParseRecord(sc.Bytes()); err != nil {
		return nil, true, fmt.Errorf("validate %s:1: %w", path, err)
	}
	for line++; sc.Scan(); line++ {
		if _, err := parser.ParseRecord(sc.Bytes()); err != nil {
			return nil, true, fmt.Errorf("validate %s:%d: %w", path, line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, true, fmt.Errorf("read %s: %w", path, err)
	}
	return findings, true, nil
}

func openLog(path string) (io.ReadCloser, error) {
	// #nosec G304 -- path comes from an explicit command argument or the configured log directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if !strings.HasSuffix(path, ".zst") {
		return f, nil
	}
	zr, err := zstd.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open zstd %s: %w", path, err)
	}
	return &zstdLog{Decoder: zr, file: f}, nil
}

type zstdLog struct {
	*zstd.Decoder

	file *os.File
}

func (z *zstdLog) Close() error {
	z.Decoder.Close()
	return z.file.Close()
}

func checkMessage(path string, line int, harness string, message []byte, openCodeMethods map[string]opencodedto.Method) *finding {
	if len(message) != 0 && message[0] == '"' {
		var reason string
		if err := json.Unmarshal(message, &reason); err == nil {
			return &finding{
				path:    path,
				line:    line,
				harness: harness,
				dto:     "relay diagnostic",
				err:     errors.New(reason),
				hint:    "inspect: backend/internal/agent/relay/relay_v2.py",
			}
		}
	}
	var err error
	var dto string
	switch harness {
	case "claude":
		dto, err = checkClaude(message)
	case "codex":
		dto, err = checkCodex(message)
	case "pi":
		dto, err = checkPi(message)
	case "opencode":
		dto, err = checkOpenCode(message, openCodeMethods)
	default:
		return &finding{
			path:    path,
			line:    line,
			harness: harness,
			dto:     "harness dispatch",
			err:     fmt.Errorf("unrecognized harness %q; add its DTO and checker dispatch", harness),
		}
	}
	if err == nil {
		return nil
	}
	return &finding{path: path, line: line, harness: harness, dto: dto, err: err}
}

func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func checkClaude(data []byte) (string, error) {
	var probe claudedto.OutputTypeProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "OutputTypeProbe", err
	}
	var dst any
	switch probe.Type {
	case claudedto.OutputAssistant:
		dst = &claudedto.OutputAssistantMsg{}
	case claudedto.OutputUser:
		dst = &claudedto.OutputUserMsg{}
	case claudedto.OutputResult:
		dst = &claudedto.OutputResultMsg{}
	case claudedto.OutputSystem:
		subtype := claudedto.SystemSubtype(probe.Subtype)
		if subtype == claudedto.SystemInit {
			dst = &claudedto.OutputInitMsg{}
		} else {
			if _, ok := knownClaudeSystemSubtypes[subtype]; !ok {
				return "OutputSystemMsg", fmt.Errorf("unrecognized Claude Code system subtype %q; add its DTO and checker dispatch", probe.Subtype)
			}
			dst = &claudedto.OutputSystemMsg{}
		}
	case claudedto.OutputStreamEvent:
		dst = &claudedto.OutputStreamEventMsg{}
	case claudedto.OutputRateLimitEvent:
		dst = &claudedto.OutputRateLimitEventMsg{}
	case claudedto.OutputToolProgress:
		dst = &claudedto.OutputToolProgressMsg{}
	case claudedto.OutputAuthStatus:
		dst = &claudedto.OutputAuthStatusMsg{}
	case claudedto.OutputToolUseSummary:
		dst = &claudedto.OutputToolUseSummaryMsg{}
	case claudedto.OutputPromptSuggestion:
		dst = &claudedto.OutputPromptSuggestionMsg{}
	case claudedto.OutputControlRequest:
		dst = &claudedto.OutputControlRequestMsg{}
	case claudedto.OutputControlResponse:
		// Task logs include both relay directions. Claude's control response is
		// an input DTO, even when it is preserved in an agent record.
		dst = &claudedto.InputControlResponseMsg{}
	case claudedto.OutputControlCancelRequest:
		dst = &claudedto.OutputControlCancelRequestMsg{}
	case claudedto.OutputStreamlinedText:
		dst = &claudedto.OutputStreamlinedTextMsg{}
	case claudedto.OutputStreamlinedToolUseSummary:
		dst = &claudedto.OutputStreamlinedToolUseSummaryMsg{}
	default:
		return "OutputTypeProbe", fmt.Errorf("unrecognized Claude Code output type %q; add its DTO and checker dispatch", probe.Type)
	}
	return fmt.Sprintf("%T", dst), strictDecode(data, dst)
}

func checkCodex(data []byte) (string, error) {
	var msg codexdto.JSONRPCMessage
	if err := strictDecode(data, &msg); err != nil {
		return "JSONRPCMessage", err
	}
	if msg.Method == "" {
		if msg.ID != nil {
			return "JSONRPCMessage", nil
		}
		return "JSONRPCMessage", errors.New("codex JSON-RPC message is missing method")
	}
	if isEmptyJSON(msg.Params) {
		return "JSONRPCMessage", fmt.Errorf("codex notification %q is missing params", msg.Method)
	}
	var dst any
	switch msg.Method {
	case codexdto.MethodThreadStarted:
		dst = &codexdto.ThreadStartedNotification{}
	case codexdto.MethodTurnStarted:
		dst = &codexdto.TurnStartedNotification{}
	case codexdto.MethodTurnCompleted:
		dst = &codexdto.TurnCompletedNotification{}
	case codexdto.MethodItemStarted:
		dst = &codexdto.ItemStartedNotification{}
	case codexdto.MethodItemCompleted:
		dst = &codexdto.ItemCompletedNotification{}
	case codexdto.MethodItemDelta:
		dst = &codexdto.AgentMessageDeltaNotification{}
	case codexdto.MethodTokenUsageUpdated:
		dst = &codexdto.ThreadTokenUsageUpdatedNotification{}
	case codexdto.MethodReasoningSummaryTextDelta:
		dst = &codexdto.ReasoningSummaryTextDeltaNotification{}
	case codexdto.MethodCommandOutputDelta:
		dst = &codexdto.CommandExecutionOutputDeltaNotification{}
	case codexdto.MethodMcpToolCallProgress:
		dst = &codexdto.McpToolCallProgressNotification{}
	case codexdto.MethodThreadStatusChanged:
		dst = &codexdto.ThreadStatusChangedNotification{}
	case codexdto.MethodModelRerouted:
		dst = &codexdto.ModelReroutedNotification{}
	case codexdto.MethodMcpServerStatusUpdated:
		dst = &codexdto.McpServerStatusUpdatedNotification{}
	case codexdto.MethodAccountRateLimitsUpdated:
		dst = &codexdto.AccountRateLimitsUpdatedNotification{}
	case codexdto.MethodSkillsChanged:
		dst = &codexdto.SkillsChangedNotification{}
	case codexdto.MethodErrorNotification:
		dst = &codexdto.ErrorNotification{}
	default:
		return "JSONRPCMessage", fmt.Errorf("unrecognized Codex notification method %q; add its DTO and checker dispatch", msg.Method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(msg.Params, dst)
}

func checkPi(data []byte) (string, error) {
	var probe struct {
		Type pidto.EventType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "LineProbe", err
	}
	var dst any
	switch probe.Type {
	case pidto.EventAgentStart:
		dst = &pidto.AgentStartEvent{}
	case pidto.EventAgentEnd:
		dst = &pidto.AgentEndEvent{}
	case pidto.EventAgentSettled:
		dst = &pidto.AgentSettledEvent{}
	case pidto.EventAutoRetryStart:
		dst = &pidto.AutoRetryStartEvent{}
	case pidto.EventAutoRetryEnd:
		dst = &pidto.AutoRetryEndEvent{}
	case pidto.EventCompactionStart:
		dst = &pidto.CompactionStartEvent{}
	case pidto.EventCompactionEnd:
		dst = &pidto.CompactionEndEvent{}
	case pidto.EventEntryAppended:
		dst = &pidto.EntryAppendedEvent{}
	case pidto.EventQueueUpdate:
		dst = &pidto.QueueUpdateEvent{}
	case pidto.EventSummarizationRetryScheduled:
		dst = &pidto.SummarizationRetryScheduledEvent{}
	case pidto.EventSummarizationRetryAttemptStart:
		dst = &pidto.SummarizationRetryAttemptStartEvent{}
	case pidto.EventSummarizationRetryFinished:
		dst = &pidto.SummarizationRetryFinishedEvent{}
	case pidto.EventThinkingLevelChanged:
		dst = &pidto.ThinkingLevelChangedEvent{}
	case pidto.EventTurnStart:
		dst = &pidto.TurnStartEvent{}
	case pidto.EventTurnEnd:
		dst = &pidto.TurnEndEvent{}
	case pidto.EventMessageStart:
		dst = &pidto.MessageStartEvent{}
	case pidto.EventMessageUpdate:
		dst = &pidto.MessageUpdateEvent{}
	case pidto.EventMessageEnd:
		dst = &pidto.MessageEndEvent{}
	case pidto.EventToolExecStart:
		dst = &pidto.ToolExecStartEvent{}
	case pidto.EventToolExecUpdate:
		dst = &pidto.ToolExecUpdateEvent{}
	case pidto.EventToolExecEnd:
		dst = &pidto.ToolExecEndEvent{}
	case pidto.EventExtensionUI:
		dst = &pidto.ExtensionUIRequest{}
	case pidto.EventResponse:
		dst = &pidto.Response{}
	case pidto.CmdPrompt:
		dst = &pidto.PromptCmd{}
	case pidto.CmdSetModel:
		dst = &pidto.SetModelCmd{}
	case pidto.CmdSetThinking:
		dst = &pidto.SetThinkingCmd{}
	case pidto.CmdGetState:
		dst = &pidto.GetStateCmd{}
	case pidto.CmdCompact:
		dst = &pidto.CompactCmd{}
	default:
		return "LineProbe", fmt.Errorf("unrecognized Pi event or command type %q; add its DTO and checker dispatch", probe.Type)
	}
	dto := fmt.Sprintf("%T", dst)
	if err := strictDecode(data, dst); err != nil {
		return dto, err
	}
	return dto, validatePiStrictContentBlocks(probe.Type, data)
}

func checkOpenCode(data []byte, methods map[string]opencodedto.Method) (string, error) {
	var probe opencodedto.MessageProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "MessageProbe", err
	}
	if probe.Type != "" {
		return checkOpenCodeInjection(probe.Type, data)
	}

	var msg opencodedto.JSONRPCMessage
	if err := strictDecode(data, &msg); err != nil {
		return "JSONRPCMessage", err
	}
	if msg.Method != "" {
		if isEmptyJSON(msg.Params) {
			return "JSONRPCMessage", fmt.Errorf("OpenCode request or notification %q is missing params", msg.Method)
		}
		if msg.ID == nil {
			return checkOpenCodeNotification(msg.Method, msg.Params)
		}
		dto, err := checkOpenCodeRequest(msg.Method, msg.Params)
		if err != nil {
			return dto, err
		}
		if methods != nil {
			methods[string(msg.ID)] = msg.Method
		}
		return dto, nil
	}
	if msg.ID == nil {
		return "JSONRPCMessage", errors.New("OpenCode JSON-RPC message is missing method and id")
	}
	if methods == nil {
		return "JSONRPCMessage", fmt.Errorf("unmatched OpenCode response id %s", msg.ID)
	}
	method, ok := methods[string(msg.ID)]
	delete(methods, string(msg.ID))
	if !ok {
		return "JSONRPCMessage", fmt.Errorf("unmatched OpenCode response id %s", msg.ID)
	}
	if msg.Error != nil {
		return "JSONRPCMessage", nil
	}
	if isEmptyJSON(msg.Result) {
		return "JSONRPCMessage", fmt.Errorf("OpenCode response for %q is missing result", method)
	}
	return checkOpenCodeResponse(method, msg.Result)
}

func checkOpenCodeRequest(method opencodedto.Method, params json.RawMessage) (string, error) {
	var dst any
	switch method {
	case opencodedto.MethodInitialize:
		dst = &opencodedto.InitializeParams{}
	case opencodedto.MethodSessionNew:
		dst = &opencodedto.SessionNewParams{}
	case opencodedto.MethodSessionLoad:
		dst = &opencodedto.SessionLoadParams{}
	case opencodedto.MethodSessionPrompt:
		dst = &opencodedto.SessionPromptParams{}
	case opencodedto.MethodSessionSetModel:
		dst = &opencodedto.SetSessionModelParams{}
	case opencodedto.MethodSessionSetConfigOption:
		dst = &opencodedto.SetSessionConfigOptionParams{}
	case opencodedto.MethodSessionRequestPermission:
		dst = &opencodedto.PermissionRequestParams{}
	case opencodedto.MethodSessionCancel, opencodedto.MethodSessionSetMode:
		return "JSONRPCMessage", fmt.Errorf("OpenCode request %q has no genai DTO; add its DTO and checker strict decode", method)
	default:
		return "JSONRPCMessage", fmt.Errorf("unrecognized OpenCode request method %q; add its DTO and checker dispatch", method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(params, dst)
}

func checkOpenCodeInjection(typ string, data []byte) (string, error) {
	var dst any
	switch typ {
	case "caic_session":
		dst = &agent.MetaSessionMessage{}
	case "caic_init":
		dst = &caicopencode.CaicInit{}
	case "caic_diff_stat":
		dst = &agent.DiffStatMessage{}
	case "caic_exit":
		dst = &agent.ExitMessage{}
	default:
		return "caic injection", fmt.Errorf("unrecognized OpenCode caic injection type %q; add its DTO and checker dispatch", typ)
	}
	return fmt.Sprintf("%T", dst), strictDecode(data, dst)
}

func isEmptyJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func checkOpenCodeNotification(method opencodedto.Method, params json.RawMessage) (string, error) {
	if method == opencodedto.MethodSessionRequestPermission {
		return "*opencode.PermissionRequestParams", strictDecode(params, &opencodedto.PermissionRequestParams{})
	}
	if method != opencodedto.MethodSessionUpdate {
		return "JSONRPCMessage", fmt.Errorf("unrecognized OpenCode notification method %q; add its DTO and checker dispatch", method)
	}

	var updateParams opencodedto.SessionUpdateParams
	if err := strictDecode(params, &updateParams); err != nil {
		return "*opencode.SessionUpdateParams", err
	}
	if isEmptyJSON(updateParams.Update) {
		return "*opencode.SessionUpdateParams", errors.New("session/update params are missing update")
	}
	var probe opencodedto.UpdateProbe
	if err := json.Unmarshal(updateParams.Update, &probe); err != nil {
		return "UpdateProbe", err
	}
	var dst any
	switch probe.SessionUpdate {
	case opencodedto.UpdateAgentMessageChunk:
		dst = &opencodedto.AgentMessageChunkUpdate{}
	case opencodedto.UpdateAgentThoughtChunk:
		dst = &opencodedto.AgentThoughtChunkUpdate{}
	case opencodedto.UpdateUserMessageChunk:
		dst = &opencodedto.UserMessageChunkUpdate{}
	case opencodedto.UpdateToolCall:
		dst = &opencodedto.ToolCallUpdate{}
	case opencodedto.UpdateToolCallUpdate:
		dst = &opencodedto.ToolCallUpdateUpdate{}
	case opencodedto.UpdatePlan:
		dst = &opencodedto.PlanUpdate{}
	case opencodedto.UpdateUsageUpdate:
		dst = &opencodedto.UsageUpdateUpdate{}
	case opencodedto.UpdateCurrentModeUpdate:
		dst = &opencodedto.CurrentModeUpdate{}
	case opencodedto.UpdateAvailableCommandsUpdate:
		dst = &opencodedto.AvailableCommandsUpdate{}
	case opencodedto.UpdateSessionInfoUpdate, opencodedto.UpdateConfigOptionUpdate:
		return "UpdateProbe", fmt.Errorf("OpenCode session update %q has no genai DTO; add its DTO and checker strict decode", probe.SessionUpdate)
	default:
		return "UpdateProbe", fmt.Errorf("unrecognized OpenCode session update %q; add its DTO and checker dispatch", probe.SessionUpdate)
	}
	return fmt.Sprintf("%T", dst), strictDecode(updateParams.Update, dst)
}

func checkOpenCodeResponse(method opencodedto.Method, result json.RawMessage) (string, error) {
	var dst any
	switch method {
	case opencodedto.MethodInitialize:
		dst = &opencodedto.InitializeResult{}
	case opencodedto.MethodSessionNew, opencodedto.MethodSessionLoad:
		dst = &opencodedto.SessionNewResult{}
	case opencodedto.MethodSessionPrompt:
		dst = &opencodedto.PromptResult{}
	case opencodedto.MethodSessionRequestPermission:
		dst = &opencodedto.PermissionResponseResult{}
	case opencodedto.MethodSessionSetConfigOption:
		dst = &opencodedto.SetSessionConfigOptionResult{}
	case opencodedto.MethodSessionSetModel, opencodedto.MethodSessionCancel, opencodedto.MethodSessionSetMode:
		return "JSONRPCMessage", fmt.Errorf("OpenCode response for %q has no genai DTO; add its DTO and checker strict decode", method)
	default:
		return "JSONRPCMessage", fmt.Errorf("unrecognized OpenCode response method %q; add its DTO and checker dispatch", method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(result, dst)
}

func dedupe(in []finding) []finding {
	seen := map[string]struct{}{}
	out := make([]finding, 0, len(in))
	for _, f := range in {
		key := strings.Join([]string{f.harness, f.dto, f.err.Error()}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path == out[j].path {
			return out[i].line < out[j].line
		}
		return out[i].path < out[j].path
	})
	return out
}
