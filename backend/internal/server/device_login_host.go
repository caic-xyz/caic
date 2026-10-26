// Adapts device pairing to caic login policy, session delivery and revocation, and durable audits.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/maruel/gomode/devicelogin"
)

type deviceLoginHost struct {
	auth  *authHandlers
	audit *auditStore
}

func (h *deviceLoginHost) Enabled() bool {
	return h.auth.store != nil && (h.auth.githubOAuth != nil || h.auth.gitlabOAuth != nil || h.auth.googleOAuth != nil)
}

func (h *deviceLoginHost) ClientIP(r *http.Request) netip.Addr {
	ip, _ := netip.ParseAddr(httpLogContextFromRequest(r).clientIP)
	return ip
}

func (h *deviceLoginHost) VerificationURL(r *http.Request) string {
	return h.auth.hostState.ExternalURL(r) + "/device"
}

func (h *deviceLoginHost) Identify(r *http.Request) (string, error) {
	u, ok := auth.UserFromContext(r.Context())
	sid := auth.SessionIDFromContext(r.Context())
	if !ok || sid == "" {
		return "", errors.New("sign in required")
	}
	if expected := r.Header.Get("X-Caic-Session"); expected != "" && expected != sid {
		return "", errors.New("session changed")
	}
	// The shared CSRF proof binds this opaque subject, including its session.
	return u.ID + "\x00" + sid, nil
}

func (h *deviceLoginHost) Eligible(_ context.Context, id string) error {
	id, _, ok := strings.Cut(id, "\x00")
	if !ok {
		return errors.New("invalid approving session")
	}
	u, ok := h.auth.store.FindByID(id)
	if !ok {
		return errors.New("user unavailable")
	}
	return h.auth.checkLogin(u.Provider, u.Username)
}

func (h *deviceLoginHost) Audit(ctx context.Context, event *devicelogin.AuditEvent) error {
	return h.audit.recordDeviceLogin(ctx, event)
}

func (h *deviceLoginHost) CreateSession(r *http.Request, subject string) (devicelogin.Session, error) {
	id, _, ok := strings.Cut(subject, "\x00")
	if !ok {
		return nil, errors.New("invalid approving session")
	}
	u, ok := h.auth.store.FindByID(id)
	if !ok {
		return nil, errors.New("user unavailable")
	}
	token, session, err := h.auth.issueSession(r, &u, "device")
	if err != nil {
		return nil, err
	}
	return &deviceSession{handlers: h.auth, request: r, session: session, token: token}, nil
}

type deviceSession struct {
	handlers *authHandlers
	request  *http.Request
	session  auth.Session
	token    string
}

func (s *deviceSession) SessionID() string { return s.session.ID }
func (s *deviceSession) WriteResponse(w http.ResponseWriter) error {
	s.handlers.writeSessionCookie(w, s.request, s.token, s.session.ExpiresAt)
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(struct{}{})
}
func (s *deviceSession) Revoke(_ context.Context) error {
	return s.handlers.store.RevokeSession(s.session.UserID, s.session.ID)
}
