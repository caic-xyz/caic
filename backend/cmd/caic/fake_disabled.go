//go:build !e2e

package main

import (
	"context"
	"errors"

	"github.com/caic-xyz/caic/backend/internal/server"
)

const isFakeMode = false

func serveFake(ctx context.Context, addr, rootDir string, cfg *server.Config) error {
	return errors.New("fake mode is not enabled in this build; use -tags e2e")
}
