// User-facing MCP client grant management handlers.

package server

import (
	"context"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func (s *Router) listMCPGrants(ctx context.Context, _ *api.EmptyReq) (*v1.MCPGrantsResp, error) {
	if !s.authEnabled() || s.mcpOAuth == nil {
		return &v1.MCPGrantsResp{}, nil
	}
	now := time.Now()
	grants := s.mcpOAuth.listUserGrants(userIDFromCtx(ctx))
	resp := make([]v1.MCPGrantResp, len(grants))
	for i := range grants {
		resp[i] = mcpGrantResponse(&grants[i], now)
	}
	return &v1.MCPGrantsResp{Grants: resp}, nil
}

func (s *Router) revokeMCPGrant(ctx context.Context, req *v1.RevokeMCPGrantReq) (*v1.StatusResp, error) {
	if !s.authEnabled() || s.mcpOAuth == nil {
		return nil, api.NotFound("MCP grant")
	}
	if !s.mcpOAuth.revokeUserGrant(userIDFromCtx(ctx), req.GrantID) {
		return nil, api.NotFound("MCP grant")
	}
	if err := s.mcpOAuth.saveRefreshTokens(); err != nil {
		return nil, api.InternalError("save MCP grant revocation: " + err.Error())
	}
	return &v1.StatusResp{Status: "ok"}, nil
}

func mcpGrantResponse(grant *mcpOAuthGrant, now time.Time) v1.MCPGrantResp {
	status := v1.MCPGrantStatusActive
	if !grant.RevokedAt.IsZero() {
		status = v1.MCPGrantStatusRevoked
	} else if now.After(grant.ExpiresAt) {
		status = v1.MCPGrantStatusExpired
	}
	return v1.MCPGrantResp{
		ID:         grant.ID,
		ClientID:   grant.ClientID,
		ClientName: grant.ClientName,
		Scopes:     strings.Fields(grant.Scope),
		Resource:   grant.Resource,
		CreatedAt:  grant.CreatedAt,
		LastUsedAt: grant.LastUsedAt,
		ExpiresAt:  grant.ExpiresAt,
		RevokedAt:  grant.RevokedAt,
		Status:     status,
	}
}
