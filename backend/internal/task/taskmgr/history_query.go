// Retains bounded per-log answers and folds cold-history queries.

package taskmgr

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

var (
	// ErrToolUseAmbiguous reports a provider tool-use ID recorded by more than
	// one source, which a query without a source cannot resolve.
	ErrToolUseAmbiguous = errors.New("tool use ID is ambiguous; specify its source")
	// ErrToolUseNotFound reports a tool-use ID absent from the task's history.
	ErrToolUseNotFound = errors.New("tool use not found")
)

const (
	// historyMemoTools bounds the retained tool inputs per entry.
	historyMemoTools = 8
	// historyMemoToolBytes bounds the total retained tool input size per entry.
	historyMemoToolBytes = 256 << 10
)

// historyIdentity identifies one physical log so a retained answer is reused
// only while that log cannot have changed. It is the size and modification time
// discipline the task-log header cache uses.
type historyIdentity struct {
	path    string
	size    int64
	modTime time.Time
}

// historyTool is one retained tool input answer, or the typed outcome deciding
// that no input can be reported.
type historyTool struct {
	key   agent.ToolKey
	input *agent.ToolUseMessage
	err   error
}

// historyMemo retains cold-history answers for one log identity. A cold query
// decodes the whole log, so the owning Entry holds historyMu for the duration of
// a query: a repeated query reuses the answer, and a query for a different
// question waits instead of decoding the log a second time in parallel.
type historyMemo struct {
	identity historyIdentity
	last     agent.Message
	haveLast bool
	tools    []historyTool // Newest first, bounded by count and input bytes.
}

// outcome returns the retained answer for key, and whether one exists.
func (memo *historyMemo) outcome(key *agent.ToolKey) (*historyTool, bool) {
	for i, t := range memo.tools {
		if t.key == *key {
			return &memo.tools[i], true
		}
	}
	return nil, false
}

// retainOutcome records one answer for key as the newest and trims the oldest
// answers beyond the count and byte bounds. An input larger than the whole byte
// budget is not retained, so it cannot pin its size for the life of the log.
func (memo *historyMemo) retainOutcome(key *agent.ToolKey, input *agent.ToolUseMessage, err error) {
	if input != nil && len(input.Input) > historyMemoToolBytes {
		return
	}
	kept := make([]historyTool, 0, len(memo.tools)+1)
	kept = append(kept, historyTool{key: *key, input: input, err: err})
	for _, t := range memo.tools {
		if t.key != *key {
			kept = append(kept, t)
		}
	}
	size := 0
	for i, t := range kept {
		if t.input != nil {
			size += len(t.input.Input)
		}
		if i > 0 && (i >= historyMemoTools || size > historyMemoToolBytes) {
			kept = kept[:i]
			break
		}
	}
	memo.tools = kept
}

// inMemoryMessages adapts a live task's snapshot to the query iterator shape.
// The snapshot is taken when the sequence is created.
func inMemoryMessages(ctx context.Context, t *taskrun.Task) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		for message := range t.ForwardMessages() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if !yield(message, nil) {
				return
			}
		}
	}
}

// foldLastAgentMessage keeps the newest agent output of an ordered sequence.
func foldLastAgentMessage(ctx context.Context, messages iter.Seq2[agent.Message, error]) (agent.Message, error) {
	var last agent.Message
	for message, err := range messages {
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if isAgentOutput(message) {
			last = message
		}
	}
	return last, nil
}

// foldToolInput resolves one tool input from an ordered sequence, keeping the
// newest match of the requested origin.
func foldToolInput(ctx context.Context, messages iter.Seq2[agent.Message, error], query *ToolInputQuery) (*agent.ToolUseMessage, error) {
	scoped := !query.Source.IsZero()
	wanted := agent.ToolKeyForSource(&query.Source, query.ToolUseID)
	var found *agent.ToolUseMessage
	var foundKey agent.ToolKey
	for message, err := range messages {
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		toolUse, ok := message.(*agent.ToolUseMessage)
		if !ok || toolUse.ToolUseID != query.ToolUseID {
			continue
		}
		key := agent.ToolKeyForSource(toolUse.Provenance(), query.ToolUseID)
		if scoped {
			if key == wanted {
				found = toolUse
			}
			continue
		}
		if found != nil && key != foundKey {
			return nil, ErrToolUseAmbiguous
		}
		found, foundKey = toolUse, key
	}
	if found == nil {
		return nil, ErrToolUseNotFound
	}
	return found, nil
}

// isAgentOutput reports whether m is agent output the last-message query answers
// with: a text, question, or result carrying a payload and no child ownership.
func isAgentOutput(m agent.Message) bool {
	if m.Provenance().IsChild() {
		return false
	}
	switch m := m.(type) {
	case *agent.ResultMessage:
		return m.Result != ""
	case *agent.AskMessage:
		return len(m.Questions) > 0
	case *agent.TextMessage:
		return m.Text != ""
	default:
		return false
	}
}

// historyIdentityOf reports the identity of the entry's physical log.
func historyIdentityOf(entry *Entry) (historyIdentity, error) {
	path := entry.LogPath.Get()
	if path == "" {
		return historyIdentity{}, taskslog.ErrNoLog
	}
	info, err := os.Stat(path)
	if err != nil {
		return historyIdentity{}, fmt.Errorf("stat task log: %w", err)
	}
	return historyIdentity{path: path, size: info.Size(), modTime: info.ModTime()}, nil
}
