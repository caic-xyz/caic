// Package v1 defines version 1 of the persisted users, account fences, and host sessions schemas.
package v1

import "time"

// Version is the version field value of every file in this schema.
const Version = 1

// Role is a persisted application role. An absent role means RoleMember.
type Role string

// Persisted application roles.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Provider identifies the stored login identity provider.
type Provider string

// User is the on-disk JSON representation of a user.
type User struct {
	ID           string    `json:"id"`
	Role         Role      `json:"role,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
	AccountEpoch int       `json:"accountEpoch,omitempty"`
	Provider     Provider  `json:"provider"`
	ProviderID   string    `json:"providerID"`
	Username     string    `json:"username"`
	AvatarURL    string    `json:"avatarURL,omitempty"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	TokenExpiry  time.Time `json:"tokenExpiry"`
	CreatedAt    time.Time `json:"createdAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}

// UsersFile is the on-disk JSON structure.
type UsersFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}
