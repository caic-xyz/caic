// Coordinates account roles, safe identity listing, durable fences, runtime suspension, and grant revocation.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/gomodedev/gomode/oauth/oauthserver"
)

type accountService struct {
	store *auth.Store
	tasks *taskmgr.Manager
	oauth *oauthserver.Server
	// Account transitions are infrequent. One coordinator prevents re-enablement
	// from overtaking an unfinished suspension or failed grant revocation.
	mu sync.Mutex
}

func (s *accountService) setEnabled(ctx context.Context, id string, enabled bool) (mcpschema.AccountEnabledOutput, error) {
	var out mcpschema.AccountEnabledOutput
	if s.store == nil {
		return out, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "account authentication is not configured"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(ctx, false); err != nil {
		return out, err
	}
	target, found := s.store.FindByID(id)
	if !found {
		return out, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "account not found"}
	}
	if enabled && !target.Disabled {
		return mcpschema.AccountEnabledOutput{UserID: id, Enabled: true}, nil
	}
	if !enabled {
		if err := s.store.SetDisabled(id, true); err != nil {
			return out, err
		}
	}
	// Once the durable fence is committed, cancellation of the administrator's
	// own connection cannot leave suspension dependent on that connection.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	stopErr := s.tasks.SuspendOwner(work, id)
	var grantErr error
	if s.oauth != nil {
		grantErr = s.oauth.RevokeAllUserGrants(id)
	}
	if err := errors.Join(stopErr, grantErr); err != nil {
		return out, fmt.Errorf("account remains disabled; suspension or grant revocation is unconfirmed; retry: %w", err)
	}
	if enabled {
		if err := s.store.SetDisabled(id, false); err != nil {
			return out, fmt.Errorf("account remains disabled: %w", err)
		}
	}
	return mcpschema.AccountEnabledOutput{UserID: id, Enabled: enabled, Suspended: true}, nil
}

func (s *accountService) list(ctx context.Context) (mcpschema.AccountsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(ctx, false); err != nil {
		return mcpschema.AccountsOutput{}, err
	}
	out := mcpschema.AccountsOutput{Accounts: []mcpschema.Account{}}
	users := s.store.Users()
	for i := range users {
		u := &users[i]
		out.Accounts = append(out.Accounts, mcpschema.Account{UserID: u.ID, Provider: mcpschema.AccountProvider(u.Provider), ProviderID: u.ProviderID, Username: u.Username, Role: mcpschema.AccountRole(u.Role), Enabled: !u.Disabled})
	}
	return out, nil
}

func (s *accountService) setRole(ctx context.Context, id string, role mcpschema.ApplicationRole) (mcpschema.AccountRoleOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(ctx, true); err != nil {
		return mcpschema.AccountRoleOutput{}, err
	}
	if err := s.store.SetRole(id, auth.Role(role)); err != nil {
		return mcpschema.AccountRoleOutput{}, err
	}
	return mcpschema.AccountRoleOutput{UserID: id, Role: role}, nil
}

// authorize is called under the account coordinator so role and admission
// changes cannot overtake one another. Task principals never inherit human roles.
func (s *accountService) authorize(ctx context.Context, superOnly bool) error {
	p := requestPrincipalFromContext(ctx)
	if s.store != nil && p.kind == principalUser && ctx.Err() == nil {
		current, active := s.store.ActiveUser(p.user.ID)
		if active && current.AccountEpoch == p.user.AccountEpoch && current.Role.IsAdmin() && (!superOnly || current.Role == auth.RoleSuperAdmin) {
			return nil
		}
	}
	return &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: "current administrator authority required; role changes require super_admin"}
}
