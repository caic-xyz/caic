// OAuth dynamic client registration (RFC 7591) and registered-client storage.

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/caic-xyz/caic/backend/internal/oauth"
)

func (s *oauthServer) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.allowRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req oauth.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = oauth.TokenEndpointAuthNone
	}
	if method != oauth.TokenEndpointAuthNone {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, redirectURI := range req.RedirectURIs {
		if !validOAuthRedirectURI(redirectURI) {
			oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
			return
		}
	}
	clientID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth client id", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	now := time.Now()
	client := oauth.Client{ID: s.clientIDPrefix + clientID, Name: req.ClientName, RedirectURIs: req.RedirectURIs, TokenEndpointAuthMethod: method, CreatedAt: now}
	if err := s.registerClient(&client); err != nil {
		slog.WarnContext(r.Context(), "save oauth client registration", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	resp := oauth.RegisterResponse{ClientID: client.ID, ClientIDIssuedAt: now.Unix(), ClientName: client.Name, RedirectURIs: client.RedirectURIs, TokenEndpointAuthMethod: method}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		slog.WarnContext(r.Context(), "encode oauth registration response", "err", err)
	}
}

func (s *oauthServer) oauthClient(id string) oauth.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Clients[id]
}

func (s *oauthServer) registerClient(client *oauth.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Clients[client.ID] = *client
	return s.state.Save()
}

func validOAuthRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func clientDisplayName(client *oauth.Client) string {
	if client.Name != "" {
		return client.Name
	}
	if client.ID != "" {
		return client.ID
	}
	return "remote OAuth client"
}
