// User store: reads and writes ~/.config/caic/users.json with atomic rename.

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/maruel/ksid"
)

const storeVersion = 1

// userRecord is the on-disk JSON representation of a user.
type userRecord struct {
	ID           string     `json:"id"`
	Provider     forge.Kind `json:"provider"`
	ProviderID   string     `json:"providerID"`
	Username     string     `json:"username"`
	AvatarURL    string     `json:"avatarURL,omitempty"`
	AccessToken  string     `json:"accessToken"`
	RefreshToken string     `json:"refreshToken,omitempty"`
	TokenExpiry  time.Time  `json:"tokenExpiry"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastSeenAt   time.Time  `json:"lastSeenAt"`
}

// usersFile is the on-disk JSON structure.
type usersFile struct {
	Version int          `json:"version"`
	Users   []userRecord `json:"users"`
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

	var rec userRecord
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
		rec = userRecord{
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
	return recordToUser(&rec), nil
}

// FindByProviderID returns the user with the given provider+ID pair, or false.
func (s *Store) FindByProviderID(provider forge.Kind, providerID string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].Provider == provider && s.file.Users[i].ProviderID == providerID {
			return recordToUser(&s.file.Users[i]), true
		}
	}
	return User{}, false
}

// FindByProvider returns the most recently seen user for the given provider, or false.
func (s *Store) FindByProvider(provider forge.Kind) (User, bool) {
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
	return recordToUser(&s.file.Users[best]), true
}

// FindByID returns the user with the given internal ID, or false.
func (s *Store) FindByID(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.file.Users {
		if s.file.Users[i].ID == id {
			return recordToUser(&s.file.Users[i]), true
		}
	}
	return User{}, false
}

func recordToUser(r *userRecord) User {
	return User(*r)
}

func loadUsersFile(path string) (*usersFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-provided, validated at startup
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &usersFile{Version: storeVersion}, nil
		}
		return nil, fmt.Errorf("read users: %w", err)
	}
	var f usersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse users: %w", err)
	}
	return &f, nil
}

func saveUsersFile(f *usersFile, path string) error {
	f.Version = storeVersion
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create users dir: %w", err)
	}
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
