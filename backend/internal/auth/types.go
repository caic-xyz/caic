// Package auth manages caic user sessions and forge OAuth login identity.
package auth

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// User represents an authenticated caic user.
type User struct {
	ID           string     // "usr_<ksid>"
	Provider     forge.Kind // "github" | "gitlab"
	ProviderID   string     // provider's numeric user ID as string
	Username     string
	AvatarURL    string
	AccessToken  string    // OAuth access token for forge API calls
	RefreshToken string    // empty for GitHub; may be set for GitLab
	TokenExpiry  time.Time // zero value means no expiry
	CreatedAt    time.Time
	LastSeenAt   time.Time
}

// Claims are the fields embedded in the JWT payload.
type Claims struct {
	UserID   string
	Provider forge.Kind
	Username string
	IssuedAt time.Time
	Expiry   time.Time
}
