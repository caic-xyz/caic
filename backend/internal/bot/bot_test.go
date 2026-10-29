// Tests comment watcher cancellation and shutdown.

package bot

import (
	"context"
	"log/slog"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

type blockingClient struct {
	started chan struct{}
	done    chan struct{}
}

func (*blockingClient) ResolveRepo(string) *RepoInfo { return nil }
func (*blockingClient) CreateTask(context.Context, task.CreateRequest) (ksid.ID, error) {
	return ksid.NewID(), nil
}
func (c *blockingClient) WatchTaskCompletion(ctx context.Context, _ ksid.ID) (state, result string, err error) {
	close(c.started)
	<-ctx.Done()
	close(c.done)
	return "", "", ctx.Err()
}
func (*blockingClient) ListPendingBotTasks() []PendingBotTask                    { return nil }
func (*blockingClient) ResolveCommenter(context.Context, string) forge.Commenter { return nil }

func TestBot(t *testing.T) {
	t.Parallel()
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		client := &blockingClient{started: make(chan struct{}), done: make(chan struct{})}
		b := New(t.Context(), slog.New(slog.DiscardHandler), client)
		b.startWatcher(ksid.NewID(), nil, "", "", 1)
		<-client.started
		b.Close()
		select {
		case <-client.done:
		default:
			t.Fatal("Close returned before watcher stopped")
		}
	})
}
