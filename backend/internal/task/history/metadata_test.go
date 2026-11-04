// Tests native-only session reconstruction agrees across inventory, cache, and full replay.

package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func TestNativeMetadataParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		harness harness.Name
		records []string
		want    agent.SessionMetadata
	}{
		{"Codex handshake rerouting and child reports", harness.Codex, []string{
			`{"t":"input","ts":1.000,"msg":{"id":7,"method":"thread/start","params":{}}}`,
			nativeLogLine(`{"id":7,"result":{"thread":{"id":"root","cliVersion":"0.154.0"},"model":"gpt-5.6","reasoningEffort":"high"}}`),
			nativeLogLine(`{"method":"thread/tokenUsage/updated","params":{"threadId":"root","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"outputTokens":200},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`),
			nativeLogLine(`{"method":"item/started","params":{"threadId":"root","item":{"id":"spawn","type":"subAgentActivity","kind":"started","agentThreadId":"child","agentPath":"/root/worker","model":"child-model"}}}`),
			nativeLogLine(`{"method":"thread/started","params":{"thread":{"id":"child","model":"child-model"}}}`),
			nativeLogLine(`{"method":"turn/started","params":{"threadId":"root","turn":{"id":"turn"}}}`),
			nativeLogLine(`{"method":"model/rerouted","params":{"threadId":"root","fromModel":"gpt-5.6","toModel":"gpt-5.6-codex"}}`),
			nativeLogLine(`{"method":"model/rerouted","params":{"threadId":"child","fromModel":"child-model","toModel":"child-rerouted"}}`),
			nativeLogLine(`{"method":"thread/tokenUsage/updated","params":{"threadId":"root","tokenUsage":{"total":{"totalTokens":1100,"inputTokens":890,"outputTokens":210},"last":{"totalTokens":100,"inputTokens":90,"outputTokens":10}}}}`),
			nativeLogLine(`{"method":"thread/tokenUsage/updated","params":{"threadId":"child","tokenUsage":{"total":{"totalTokens":35,"inputTokens":30,"outputTokens":5},"last":{"totalTokens":35,"inputTokens":30,"outputTokens":5}}}}`),
		}, agent.SessionMetadata{SessionID: "root", ReportedModel: "gpt-5.6-codex", ReportedEffort: "high", AgentVersion: "0.154.0"}},
		{"OpenCode correlated handshake", harness.OpenCode, []string{
			`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}}`,
			nativeLogLine(`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"version":"1.2.3"}}}`),
			`{"t":"input","ts":2.000,"msg":{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/workspace","mcpServers":[]}}}`,
			nativeLogLine(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"openai/gpt-5"},"configOptions":[{"id":"effort","type":"select","currentValue":"high","options":[{"value":"high"}]}]}}`),
			`{"t":"input","ts":3.000,"msg":{"id":3,"method":"session/set_config_option","params":{"sessionId":"session-1","configId":"model","value":"openai/served"}}}`,
			nativeLogLine(`{"id":3,"result":{"configOptions":[{"id":"model","currentValue":"openai/served"}]}}`),
			nativeLogLine(`{"method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"effort","currentValue":"low"}]}}}`),
			`{"t":"session","session_id":"session-1","model":"stale","reported_effort":"stale"}`,
			`{"t":"input","ts":4.000,"msg":{"id":1,"method":"initialize","params":{}}}`,
			nativeLogLine(`{"id":1,"result":{"agentInfo":{"version":"1.2.4"}}}`),
			`{"t":"input","ts":5.000,"msg":{"id":2,"method":"session/load","params":{"sessionId":"session-1"}}}`,
			nativeLogLine(`{"id":2,"result":{}}`),
		}, agent.SessionMetadata{SessionID: "session-1", ReportedModel: "openai/served", ReportedEffort: "low", AgentVersion: "1.2.4"}},
		{"Pi native state", harness.Pi, []string{
			`{"t":"input","ts":1.000,"msg":{"type":"get_state"}}`,
			nativeLogLine(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","model":{"provider":"openai-codex","id":"gpt-5.6-terra"},"thinkingLevel":"high"}}`),
			`{"t":"input","ts":2.000,"msg":{"type":"set_model","provider":"openrouter","modelId":"selected"}}`,
			nativeLogLine(`{"type":"response","command":"set_model","success":true,"data":{"provider":"openrouter","id":"selected"}}`),
			`{"t":"session","session_id":"ses-1","model":"stale","reported_effort":"low"}`,
			`{"t":"input","ts":3.000,"msg":{"type":"get_state"}}`,
			nativeLogLine(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1"}}`),
		}, agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/selected", ReportedEffort: "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, compressed := range []bool{false, true} {
				for _, cleared := range []bool{false, true} {
					t.Run(fmt.Sprintf("compressed=%t/clear=%t", compressed, cleared), func(t *testing.T) {
						t.Parallel()
						records := []string{fmt.Sprintf(`{"t":"caic_meta","version":3,"prompt":"native session","harness":%q}`, tc.harness)}
						records = append(records, tc.records...)
						want := tc.want
						if cleared {
							records = append(records, `{"t":"context_cleared"}`)
							want.SessionID = ""
						}
						records = append(records, `{"t":"result","state":"purged"}`)
						path := writePhysicalTestLog(t, compressed, records...)
						info, err := os.Stat(path)
						if err != nil {
							t.Fatal(err)
						}
						oldCache := fmt.Sprintf(`{"version":10,"log_size":%d,"log_mtime_unix_nano":%d,"task":{"state":"purged","reported_model":"stale"}}`, info.Size(), info.ModTime().UnixNano())
						if err := os.WriteFile(logHeaderCachePath(path), []byte(oldCache), 0o600); err != nil {
							t.Fatal(err)
						}
						resolver := backends.Default("", nil)
						store := NewStore(testLogger(), taskslog.NewStore(filepath.Dir(path)), resolver)
						for attempt := range 2 {
							var inventory []*LoadedTask
							var err error
							if compressed {
								inventory, err = store.LoadSettled()
							} else {
								inventory, err = store.LoadUnsettled()
							}
							if err != nil || len(inventory) != 1 {
								t.Fatalf("inventory attempt %d=%+v %v", attempt, inventory, err)
							}
							loaded := inventory[0]
							if loaded.SessionMetadata != want {
								t.Fatalf("inventory attempt %d=%+v want=%+v", attempt, loaded.SessionMetadata, want)
							}
							if loaded.Timeline != nil {
								t.Fatal("inventory retained conversation")
							}
							if _, ok := readHeaderCache(path); !ok {
								t.Fatal("terminal projection was not cached")
							}
							source, err := NewStore(testLogger(), taskslog.NewStore(filepath.Dir(path)), resolver).LoadSource(path)
							if err != nil {
								t.Fatal(err)
							}
							if err := source.LoadMessages(); err != nil {
								t.Fatal(err)
							}
							if source.SessionMetadata != want {
								t.Fatalf("full replay=%+v want=%+v", source.SessionMetadata, want)
							}
						}
					})
				}
			}
		})
	}
}
