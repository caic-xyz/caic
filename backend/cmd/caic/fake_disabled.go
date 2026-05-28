//go:build !e2e

// Stub that disables fake/e2e mode in standard builds.

package main

import (
	"context"
	"errors"

	"github.com/caic-xyz/caic/backend/internal/server"
)

const isFakeMode = false

func serveFake(ctx context.Context, addr string, cfg *server.Config, traceFile string) error {
	return errors.New("fake mode is not enabled in this build; use -tags e2e")
}
