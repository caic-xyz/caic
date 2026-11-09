// Resolves configured account authority and persists user roles, account fences, and session generations.

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/auth/data/v1"
	"github.com/maruel/ksid"
)

type usersFile struct {
	Users []User
}

// ErrAccountDisabled rejects new authority while an account is disabled.
var ErrAccountDisabled = errors.New("account is disabled")

type accountBinding struct {
	cancel context.CancelFunc
	done   chan struct{}
	work   bool
}
type ownerGenerationKey struct{}
type ownerGeneration struct {
	id    string
	epoch int
}

// Store manages the users.json file with in-memory caching.
// All methods are safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	path       string
	file       usersFile
	sessions   []Session
	bindings   map[string]map[*accountBinding]struct{}
	superAdmin string
}

// Open loads identities and resolves superAdmin from an optional provider:account-id
// selector. A disabled configured account requires operator recovery, not implicit re-enablement.
func Open(path, superAdmin string) (*Store, error) {
	if err := ValidateSuperAdmin(superAdmin); err != nil {
		return nil, err
	}
	f, err := loadUsersFile(path)
	if err != nil {
		return nil, err
	}
	for i := range f.Users {
		u := &f.Users[i]
		if string(u.Provider)+":"+u.ProviderID == superAdmin && u.Disabled {
			return nil, errors.New("configured super_admin is disabled; configure another identity and restart to recover through account administration")
		}
	}
	sessions, err := loadSessions(path + ".sessions")
	if err != nil {
		return nil, err
	}
	return &Store{path: path, file: *f, sessions: sessions, bindings: make(map[string]map[*accountBinding]struct{}), superAdmin: superAdmin}, nil
}

// UpsertUser creates or updates a user matched by (Provider, ProviderID).
// On create: generates a new "usr_<ksid>" ID and sets CreatedAt.
// On update: updates tokens, AvatarURL, Username, LastSeenAt.
// Returns the upserted User.
func (s *Store) UpsertUser(u *User) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	idx := -1
	for i := range s.file.Users {
		if s.file.Users[i].Provider == u.Provider && s.file.Users[i].ProviderID == u.ProviderID {
			idx = i
			break
		}
	}

	var rec User
	next := usersFile{Users: slices.Clone(s.file.Users)}
	if idx >= 0 {
		// Update existing.
		rec = s.file.Users[idx]
		if rec.Disabled {
			return User{}, ErrAccountDisabled
		}
		rec.Username = u.Username
		rec.AvatarURL = u.AvatarURL
		rec.AccessToken = u.AccessToken
		rec.RefreshToken = u.RefreshToken
		rec.TokenExpiry = u.TokenExpiry
		rec.LastSeenAt = now
		next.Users[idx] = rec
	} else {
		// Create new.
		rec = User{
			ID:           "usr_" + ksid.NewID().String(),
			Role:         RoleMember,
			Provider:     u.Provider,
			ProviderID:   u.ProviderID,
			Username:     u.Username,
			AvatarURL:    u.AvatarURL,
			AccessToken:  u.AccessToken,
			RefreshToken: u.RefreshToken,
			TokenExpiry:  u.TokenExpiry,
			CreatedAt:    now,
			LastSeenAt:   now,
		}
		next.Users = append(next.Users, rec)
	}

	if err := saveUsersFile(&next, s.path); err != nil {
		return User{}, err
	}
	s.file = next
	return s.effectiveUser(&rec), nil
}

// Users returns account records with configuration-owned authority resolved.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	users := slices.Clone(s.file.Users)
	for i := range users {
		users[i] = s.effectiveUser(&users[i])
	}
	return users
}

// SetRole persists an application-owned role before publishing it to readers.
// Configuration-owned super administrators cannot be changed. The final administrator
// cannot be demoted. Login and provider token refresh never assign roles.
func (s *Store) SetRole(id string, role Role) error {
	if role != RoleAdmin && role != RoleMember {
		return errors.New("role must be admin or member")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == id })
	if i < 0 {
		return os.ErrNotExist
	}
	if s.effectiveUser(&s.file.Users[i]).Role == RoleSuperAdmin {
		return errors.New("super_admin is owned by server configuration")
	}
	if s.file.Users[i].Role == role {
		return nil
	}
	if s.file.Users[i].Role == RoleAdmin && !s.file.Users[i].Disabled && role != RoleAdmin {
		n := 0
		for j := range s.file.Users {
			if s.effectiveUser(&s.file.Users[j]).Role.IsAdmin() && !s.file.Users[j].Disabled {
				n++
			}
		}
		if n == 1 {
			return errors.New("cannot demote the last administrator")
		}
	}
	next := usersFile{Users: slices.Clone(s.file.Users)}
	next.Users[i].Role = role
	if err := saveUsersFile(&next, s.path); err != nil {
		return err
	}
	s.file = next
	return nil
}

// FindByProviderID returns the user with the given provider+ID pair, or false.
func (s *Store) FindByProviderID(provider Provider, providerID string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].Provider == provider && s.file.Users[i].ProviderID == providerID {
			return s.effectiveUser(&s.file.Users[i]), true
		}
	}
	return User{}, false
}

// FindByProvider returns the most recently seen user for the given provider, or false.
func (s *Store) FindByProvider(provider Provider) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	for i := range s.file.Users {
		if s.file.Users[i].Provider == provider && s.file.Users[i].AccessToken != "" && !s.file.Users[i].Disabled {
			if best < 0 || s.file.Users[i].LastSeenAt.After(s.file.Users[best].LastSeenAt) {
				best = i
			}
		}
	}
	if best < 0 {
		return User{}, false
	}
	return s.effectiveUser(&s.file.Users[best]), true
}

// FindByID returns the user with the given internal ID, or false.
func (s *Store) FindByID(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].ID == id {
			return s.effectiveUser(&s.file.Users[i]), true
		}
	}
	return User{}, false
}

// IssueToken creates and persists a session before returning its signed credential and session metadata.
func (s *Store) IssueToken(u *User, secret []byte, ttl time.Duration, info SessionInfo) (credential string, recorded Session, retErr error) {
	return s.issueToken(u, secret, ttl, info, "")
}

// SessionUser resolves the active account and generation behind an approving session.
func (s *Store) SessionUser(userID, sid string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionUserLocked(userID, sid)
}

// IssuePairedToken persists a derived session only while its approving session
// still has authority. Approval revocation and new session publication share a lock.
func (s *Store) IssuePairedToken(userID, sid string, secret []byte, ttl time.Duration, info SessionInfo) (credential string, recorded Session, retErr error) {
	u, ok := s.SessionUser(userID, sid)
	if !ok {
		return "", Session{}, ErrAccountDisabled
	}
	return s.issueToken(&u, secret, ttl, info, sid)
}

// Sessions lists the user's unexpired sessions, newest first.
func (s *Store) Sessions(userID string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Session{}
	u := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == userID })
	if u < 0 || s.file.Users[u].Disabled {
		return out
	}
	epoch := s.file.Users[u].AccountEpoch
	now := time.Now()
	for i := range s.sessions {
		p := &s.sessions[i]
		if p.UserID == userID && p.AccountEpoch == epoch && now.Before(p.ExpiresAt) {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// RevokeSession removes one owned session, persisting before returning success.
func (s *Store) RevokeSession(userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := slices.Clone(s.sessions)
	for i := range next {
		if next[i].ID == id && next[i].UserID == userID {
			next = slices.Delete(next, i, i+1)
			if err := saveSessions(s.path+".sessions", next); err != nil {
				return err
			}
			s.sessions = next
			return nil
		}
	}
	return os.ErrNotExist
}

// ActiveUser returns the current identity only while it can authenticate.
func (s *Store) ActiveUser(id string) (User, bool) {
	u, ok := s.FindByID(id)
	return u, ok && !u.Disabled
}

// BindOwner binds work to the current account lifetime. Disablement cancels
// existing bindings. Re-enablement requires new bindings and new login sessions.
// An empty owner denotes existing operator work pending ownership migration.
func (s *Store) BindOwner(ctx context.Context, id string) (context.Context, context.CancelFunc, error) {
	return s.bindOwner(ctx, id, nil)
}

// BindUser attaches an authenticated request only to its original account generation.
func (s *Store) BindUser(ctx context.Context, u *User) (context.Context, context.CancelFunc, error) {
	return s.bindOwner(ctx, u.ID, &u.AccountEpoch)
}

// SetDisabled persists the account fence before cancelling its bound work.
// Disabled state is authoritative even if a caller cannot stop a runtime or
// revoke an external grant afterwards. Those side effects must finish before
// the caller reports suspension or allows re-enablement.
func (s *Store) SetDisabled(id string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == id })
	if i < 0 {
		return os.ErrNotExist
	}
	if s.effectiveUser(&s.file.Users[i]).Role == RoleSuperAdmin {
		return errors.New("super_admin is owned by server configuration")
	}
	if s.file.Users[i].Disabled == disabled {
		return nil
	}
	if disabled && s.file.Users[i].Role == RoleAdmin {
		n := 0
		for j := range s.file.Users {
			if s.effectiveUser(&s.file.Users[j]).Role.IsAdmin() && !s.file.Users[j].Disabled {
				n++
			}
		}
		if n == 1 {
			return errors.New("cannot disable the last administrator")
		}
	}
	next := usersFile{Users: slices.Clone(s.file.Users)}
	next.Users[i].Disabled = disabled
	if disabled {
		next.Users[i].AccountEpoch++
	}
	if err := saveUsersFile(&next, s.path); err != nil {
		return err
	}
	s.file = next
	if disabled {
		for b := range s.bindings[id] {
			b.cancel()
		}
	}
	return nil
}

// WaitOwner joins admitted owner work after its durable disablement fence.
// Cancellation stops admission; only explicit release certifies producer completion.
func (s *Store) WaitOwner(ctx context.Context, id string) error {
	s.mu.Lock()
	var pending []<-chan struct{}
	for b := range s.bindings[id] {
		if b.work {
			pending = append(pending, b.done)
		}
	}
	s.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// WithAccountAuthority validates and holds an account generation through an
// external authority publication. Publish must not call back into Store.
func (s *Store) WithAccountAuthority(id string, epoch int, publish func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == id })
	if i < 0 || s.file.Users[i].Disabled || s.file.Users[i].AccountEpoch != epoch {
		return ErrAccountDisabled
	}
	return publish()
}

func (s *Store) bindOwner(ctx context.Context, id string, epoch *int) (context.Context, context.CancelFunc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if actor, ok := UserFromContext(ctx); ok {
		actorIndex := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == actor.ID })
		if actorIndex < 0 || s.file.Users[actorIndex].Disabled || s.file.Users[actorIndex].AccountEpoch != actor.AccountEpoch {
			return nil, nil, ErrAccountDisabled
		}
	}
	if original, ok := ctx.Value(ownerGenerationKey{}).(ownerGeneration); ok {
		index := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == original.id })
		if index < 0 || s.file.Users[index].Disabled || s.file.Users[index].AccountEpoch != original.epoch {
			return nil, nil, ErrAccountDisabled
		}
	}
	if id != "" {
		i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == id })
		if i < 0 {
			return nil, nil, os.ErrNotExist
		}
		if s.file.Users[i].Disabled || (epoch != nil && s.file.Users[i].AccountEpoch != *epoch) {
			return nil, nil, ErrAccountDisabled
		}
		ctx = context.WithValue(ctx, ownerGenerationKey{}, ownerGeneration{id: id, epoch: s.file.Users[i].AccountEpoch})
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &accountBinding{cancel: cancel, done: make(chan struct{}), work: epoch == nil}
	if s.bindings[id] == nil {
		s.bindings[id] = make(map[*accountBinding]struct{})
	}
	s.bindings[id][b] = struct{}{}
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.bindings[id][b]; exists {
			delete(s.bindings[id], b)
			close(b.done)
		}
		if len(s.bindings[id]) == 0 {
			delete(s.bindings, id)
		}
	}
	return ctx, func() { cancel(); release() }, nil
}

func (s *Store) issueToken(u *User, secret []byte, ttl time.Duration, info SessionInfo, approver string) (credential string, recorded Session, retErr error) {
	now := time.Now().UTC()
	id := "ses_" + ksid.NewID().String()
	payload := jwtPayload{
		SessionID: id,
		UserID:    u.ID,
		Provider:  string(u.Provider),
		Username:  u.Username,
		IssuedAt:  now.Unix(),
		Expiry:    now.Add(ttl).Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", Session{}, fmt.Errorf("marshal jwt payload: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sigInput := jwtHeader + "." + encodedPayload
	sig := hmacSHA256(secret, sigInput)
	token := sigInput + "." + sig
	session := Session{ID: id, UserID: u.ID, AccountEpoch: u.AccountEpoch, CreatedAt: now, ExpiresAt: time.Unix(payload.Expiry, 0).UTC(), Source: info.Source, IP: info.IP, UserAgent: info.UserAgent, TokenHash: sha256.Sum256([]byte(token))}
	if err := s.addSession(&session, approver); err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

// effectiveUser derives operator authority without persisting it as an application role.
func (s *Store) effectiveUser(record *User) User {
	u := *record
	if s.superAdmin != "" && string(u.Provider)+":"+u.ProviderID == s.superAdmin {
		u.Role = RoleSuperAdmin
	}
	return u
}

func (s *Store) sessionUserLocked(userID, sid string) (User, bool) {
	index := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == userID })
	if index < 0 || s.file.Users[index].Disabled {
		return User{}, false
	}
	u := s.file.Users[index]
	for i := range s.sessions {
		session := &s.sessions[i]
		if session.ID == sid && session.UserID == userID && session.AccountEpoch == u.AccountEpoch && time.Now().Before(session.ExpiresAt) {
			return s.effectiveUser(&u), true
		}
	}
	return User{}, false
}

func (s *Store) addSession(p *Session, approver string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == p.UserID })
	if i < 0 {
		return errors.New("session user not found")
	}
	if s.file.Users[i].Disabled || s.file.Users[i].AccountEpoch != p.AccountEpoch {
		return ErrAccountDisabled
	}
	if approver != "" {
		if _, ok := s.sessionUserLocked(p.UserID, approver); !ok {
			return ErrAccountDisabled
		}
	}
	next := make([]Session, 0, len(s.sessions)+1)
	for i := range s.sessions {
		if time.Now().Before(s.sessions[i].ExpiresAt) {
			next = append(next, s.sessions[i])
		}
	}
	next = append(next, *p)
	if err := saveSessions(s.path+".sessions", next); err != nil {
		return err
	}
	s.sessions = next
	return nil
}

func (s *Store) validateSession(c *claims, token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.file.Users, func(u User) bool { return u.ID == c.UserID })
	if i < 0 || s.file.Users[i].Disabled {
		return Session{}, false
	}
	epoch := s.file.Users[i].AccountEpoch
	hash := sha256.Sum256([]byte(token))
	for i := range s.sessions {
		p := &s.sessions[i]
		if p.ID == c.SessionID && p.UserID == c.UserID && p.AccountEpoch == epoch && time.Now().Before(p.ExpiresAt) && p.ExpiresAt.Unix() == c.Expiry.Unix() && hmac.Equal(p.TokenHash[:], hash[:]) {
			return *p, true
		}
	}
	return Session{}, false
}

func loadUsersFile(path string) (*usersFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-provided, validated at startup
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &usersFile{}, nil
		}
		return nil, fmt.Errorf("read users: %w", err)
	}
	var f v1.UsersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse users: %w", err)
	}
	if f.Version != v1.Version {
		return nil, fmt.Errorf("unsupported users version %d", f.Version)
	}
	users := make([]User, len(f.Users))
	if f.Users == nil {
		users = nil
	}
	for i := range f.Users {
		r := &f.Users[i]
		var role Role
		switch r.Role {
		case "", v1.RoleMember:
			role = RoleMember
		case v1.RoleAdmin:
			role = RoleAdmin
		default:
			return nil, fmt.Errorf("invalid role for user %q", r.ID)
		}
		users[i] = User{
			ID: r.ID, Role: role, Disabled: r.Disabled, AccountEpoch: r.AccountEpoch, Provider: Provider(r.Provider), ProviderID: r.ProviderID,
			Username: r.Username, AvatarURL: r.AvatarURL,
			AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenExpiry: r.TokenExpiry,
			CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		}
	}
	return &usersFile{Users: users}, nil
}

func saveUsersFile(f *usersFile, path string) error {
	users := make([]v1.User, len(f.Users))
	if f.Users == nil {
		users = nil
	}
	for i := range f.Users {
		r := &f.Users[i]
		var role v1.Role
		if r.Role == RoleAdmin {
			role = v1.RoleAdmin
		}
		users[i] = v1.User{
			ID: r.ID, Role: role, Disabled: r.Disabled, AccountEpoch: r.AccountEpoch, Provider: v1.Provider(r.Provider), ProviderID: r.ProviderID,
			Username: r.Username, AvatarURL: r.AvatarURL,
			AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenExpiry: r.TokenExpiry,
			CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		}
	}
	data, err := json.MarshalIndent(v1.UsersFile{Version: v1.Version, Users: users}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write users: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename users: %w", err)
	}
	return nil
}
