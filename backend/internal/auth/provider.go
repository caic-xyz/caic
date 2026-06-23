// Login identity provider identifiers and forge mapping.

package auth

import "github.com/caic-xyz/caic/backend/internal/forge"

// Provider identifies the OAuth identity provider a user authenticated through.
//
// GitHub and GitLab double as code-hosting forges; Google is an identity-only
// provider with no forge backing.
type Provider string

// Supported login providers.
const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
	ProviderGoogle Provider = "google"
)

// Forge returns the code-hosting forge backing the provider and true when the
// provider is forge-backed. Identity-only providers return ("", false).
func (p Provider) Forge() (forge.Kind, bool) {
	switch p {
	case ProviderGitHub:
		return forge.KindGitHub, true
	case ProviderGitLab:
		return forge.KindGitLab, true
	default:
		return "", false
	}
}

// Label returns the human-readable provider name, or the raw value when unknown.
func (p Provider) Label() string {
	switch p {
	case ProviderGitHub:
		return "GitHub"
	case ProviderGitLab:
		return "GitLab"
	case ProviderGoogle:
		return "Google"
	default:
		return string(p)
	}
}
