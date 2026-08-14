// Login identity provider identifiers.

package auth

// Provider identifies the OAuth identity provider a user authenticated through.
type Provider string

// Supported login providers.
const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
	ProviderGoogle Provider = "google"
)

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
