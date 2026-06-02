// Package app assembles the caic backend application.
package app

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/server"
)

// New creates the caic backend server application.
func New(ctx context.Context, rootDir string, cfg *server.Config) (*server.Server, error) {
	return server.New(ctx, rootDir, cfg)
}
