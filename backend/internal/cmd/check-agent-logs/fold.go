// Checks replayed logs for missing prices and disagreements with live task state.

package main

import (
	"fmt"
	"io"

	"github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

// checkReplay loads path as task restore does, audits prices, and writes the
// per-point comparison of live ingestion and replay to out.
func checkReplay(store *history.Store, path, harness string, out io.Writer, prices *priceAudit) (*finding, error) {
	lt, err := store.LoadSource(path)
	if err == nil {
		err = lt.LoadMessages()
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	prices.observe(lt, path)
	c, err := taskrun.CheckFold(lt, out)
	if err != nil {
		return nil, fmt.Errorf("fold %s: %w", path, err)
	}
	if c.Mismatches == 0 {
		return nil, nil //nolint:nilnil // A consistent fold has no finding.
	}
	return &finding{
		path:    path,
		harness: harness,
		dto:     "activity fold",
		err:     fmt.Errorf("live ingestion and replay disagree at %d of %d replayed points", c.Mismatches, c.Replays),
		hint:    "inspect: backend/internal/task/taskrun/task.go foldMessageLocked; rerun with -fold-out to list the points",
	}, nil
}
