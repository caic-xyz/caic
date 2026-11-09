// Version 1 of the persisted host sessions file stored beside users.json.

package v1

import "time"

// Session is the on-disk JSON representation of a host login.
type Session struct {
	ID           string    `json:"id"`
	UserID       string    `json:"userID"`
	AccountEpoch int       `json:"accountEpoch,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Source       string    `json:"source"`
	IP           string    `json:"ip"`
	UserAgent    string    `json:"userAgent"`
	TokenHash    [32]byte  `json:"tokenHash"`
}

// SessionsFile is the on-disk JSON structure of the host sessions file.
type SessionsFile struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
}
