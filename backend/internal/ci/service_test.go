// Tests cancellation and shutdown of CI service background work.

package ci

import (
	"context"
	"log/slog"
	"testing"
)

func TestService(t *testing.T) {
	t.Parallel()
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		svc := NewService(slog.New(slog.DiscardHandler), nil, nil, nil)
		requestCtx, cancel := context.WithCancel(t.Context())
		started := make(chan context.Context, 1)
		done := make(chan struct{})
		svc.startWorker(requestCtx, func(ctx context.Context) {
			started <- ctx
			<-ctx.Done()
			close(done)
		})
		workerCtx := <-started
		cancel()
		if workerCtx.Err() != nil {
			t.Fatal("request cancellation stopped background CI work")
		}
		svc.Close()
		select {
		case <-done:
		default:
			t.Fatal("Close returned before worker stopped")
		}
	})
}
