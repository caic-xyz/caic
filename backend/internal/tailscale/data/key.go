// Package data defines the Tailscale API key object that caic reads.
package data

import "time"

// Key is a Tailscale API access token, auth key, OAuth client, or federated
// identity, as the Tailscale API v2 "Key" schema describes it. Fields that
// apply to one kind are empty for the others.
//
// https://tailscale.com/api#tag/keys
type Key struct {
	ID string `json:"id"`
	// Key is the secret; the API returns it only when it creates the key.
	Key string `json:"key,omitempty"`
	// KeyType is "api", "auth", "client", or "federated".
	KeyType string `json:"keyType,omitempty"`
	// ExpirySeconds is the lifetime the key was created with.
	ExpirySeconds int64     `json:"expirySeconds,omitempty"`
	Created       time.Time `json:"created,omitzero"`
	Updated       time.Time `json:"updated,omitzero"`
	// Expires is zero for a key that never expires.
	Expires time.Time `json:"expires,omitzero"`
	// Revoked is when the key was revoked; zero when it was not.
	Revoked      time.Time        `json:"revoked,omitzero"`
	Capabilities *KeyCapabilities `json:"capabilities,omitempty"`
	// Scopes are the permissions of an API access token, OAuth client, or
	// federated identity, such as "all" or "all:read".
	Scopes []string `json:"scopes,omitempty"`
	// Tags are the tags an OAuth client or federated identity must apply.
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	// Invalid is true for a revoked or expired key.
	Invalid bool `json:"invalid,omitempty"`
	// UserID is the creator; empty for keys that trust credentials created.
	UserID string `json:"userId,omitempty"`
	// Audience, Issuer, Subject, and CustomClaimRules match OIDC identity
	// tokens of a federated identity.
	Audience         string            `json:"audience,omitempty"`
	Issuer           string            `json:"issuer,omitempty"`
	Subject          string            `json:"subject,omitempty"`
	CustomClaimRules map[string]string `json:"customClaimRules,omitempty"`
}

// KeyCapabilities maps resources to an auth key's permitted actions.
type KeyCapabilities struct {
	Devices KeyDevices `json:"devices"`
}

// KeyDevices holds an auth key's permissions over devices.
type KeyDevices struct {
	Create KeyDeviceCreate `json:"create"`
}

// KeyDeviceCreate describes the devices an auth key registers.
type KeyDeviceCreate struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags,omitempty"`
	// NodeAttributes are device posture attributes set on registered devices.
	NodeAttributes map[string]any `json:"nodeAttributes,omitempty"`
}
