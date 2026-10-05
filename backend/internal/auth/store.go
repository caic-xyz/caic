// User store: reads and writes ~/.config/caic/users.json with atomic rename.

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/auth/data/v1"
	"github.com/maruel/ksid"
)

// currentVersion is the users file format version written by this store.
const currentVersion = 1

type usersFile struct {
	Version int
	Users   []User
}

// Store manages the users.json file with in-memory caching.
// All methods are safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	file usersFile
}

// Open reads or creates users.json at path.
func Open(path string) (*Store, error) {
	f, err := loadUsersFile(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, file: *f}, nil
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
	if idx >= 0 {
		// Update existing.
		rec = s.file.Users[idx]
		rec.Username = u.Username
		rec.AvatarURL = u.AvatarURL
		rec.AccessToken = u.AccessToken
		rec.RefreshToken = u.RefreshToken
		rec.TokenExpiry = u.TokenExpiry
		rec.LastSeenAt = now
		s.file.Users[idx] = rec
	} else {
		// Create new.
		rec = User{
			ID:           "usr_" + ksid.NewID().String(),
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
		s.file.Users = append(s.file.Users, rec)
	}

	if err := saveUsersFile(&s.file, s.path); err != nil {
		return User{}, err
	}
	return rec, nil
}

// FindByProviderID returns the user with the given provider+ID pair, or false.
func (s *Store) FindByProviderID(provider Provider, providerID string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].Provider == provider && s.file.Users[i].ProviderID == providerID {
			return s.file.Users[i], true
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
		if s.file.Users[i].Provider == provider && s.file.Users[i].AccessToken != "" {
			if best < 0 || s.file.Users[i].LastSeenAt.After(s.file.Users[best].LastSeenAt) {
				best = i
			}
		}
	}
	if best < 0 {
		return User{}, false
	}
	return s.file.Users[best], true
}

// FindByID returns the user with the given internal ID, or false.
func (s *Store) FindByID(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].ID == id {
			return s.file.Users[i], true
		}
	}
	return User{}, false
}

func loadUsersFile(path string) (*usersFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-provided, validated at startup
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &usersFile{Version: currentVersion}, nil
		}
		return nil, fmt.Errorf("read users: %w", err)
	}
	var f v1.UsersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse users: %w", err)
	}
	users := make([]User, len(f.Users))
	if f.Users == nil {
		users = nil
	}
	for i := range f.Users {
		r := &f.Users[i]
		users[i] = User{
			ID: r.ID, Provider: Provider(r.Provider), ProviderID: r.ProviderID,
			Username: r.Username, AvatarURL: r.AvatarURL,
			AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenExpiry: r.TokenExpiry,
			CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		}
	}
	return &usersFile{Version: f.Version, Users: users}, nil
}

func saveUsersFile(f *usersFile, path string) error {
	f.Version = currentVersion
	users := make([]v1.User, len(f.Users))
	if f.Users == nil {
		users = nil
	}
	for i := range f.Users {
		r := &f.Users[i]
		users[i] = v1.User{
			ID: r.ID, Provider: v1.Provider(r.Provider), ProviderID: r.ProviderID,
			Username: r.Username, AvatarURL: r.AvatarURL,
			AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenExpiry: r.TokenExpiry,
			CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		}
	}
	data, err := json.MarshalIndent(v1.UsersFile{Version: f.Version, Users: users}, "", "  ")
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
