// Go Mode service discovery DTOs shared with generated SDKs.

package gomode

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Settings is the service compatibility document consumed by Go Mode clients.
type Settings struct {
	Service string `json:"service"`
	// ServiceVersion is the host product version. It is informational and does
	// not participate in Go Mode compatibility checks.
	ServiceVersion string `json:"serviceVersion,omitempty"`
	// APIVersion is the Go Mode discovery schema version. A client must reject
	// manifests with an API version it does not explicitly support.
	APIVersion int              `json:"apiVersion"`
	WebShell   WebShellSettings `json:"webShell"`
}

// Validate returns an error if s is not a well-formed Go Mode discovery manifest.
func (s *Settings) Validate() error {
	return errors.Join(
		requireString("service", s.Service),
		requirePositive("apiVersion", s.APIVersion),
		s.WebShell.validate("webShell"),
	)
}

// ErrorResponse is the standard JSON error envelope for Go Mode SDK clients.
type ErrorResponse struct {
	Error   ErrorBody      `json:"error"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorBody describes a Go Mode API error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WebShellSettings describes the hosted frontend's native-shell contract.
type WebShellSettings struct {
	// BridgeVersion is the hosted-frontend/native bridge contract version. A
	// shell must reject manifests whose bridge version differs from the bridge
	// version implemented by that shell.
	BridgeVersion int                  `json:"bridgeVersion"`
	ToolGroups    []ToolGroup          `json:"toolGroups"`
	VoiceGateway  VoiceGatewaySettings `json:"voiceGateway"`
}

func (s *WebShellSettings) validate(prefix string) error {
	var errs []error
	errs = append(errs, requirePositive(prefix+".bridgeVersion", s.BridgeVersion))
	if s.ToolGroups == nil {
		errs = append(errs, fmt.Errorf("%s.toolGroups is required", prefix))
	}
	for i := range s.ToolGroups {
		errs = append(errs, s.ToolGroups[i].validate(fmt.Sprintf("%s.toolGroups[%d]", prefix, i)))
	}
	errs = append(errs, s.VoiceGateway.validate(prefix+".voiceGateway"))
	return errors.Join(errs...)
}

// ToolGroup describes a bootstrap MCP tool group advertised by the manifest.
//
// A tool group is the compatibility manifest's compact view of a Go Mode skill.
// The authoritative context, activation hints, and tool subset live in the
// skill's SKILL.md frontmatter; the manifest stays small so clients can decide
// whether native features are compatible before loading skill files.
type ToolGroup struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Endpoint    string `json:"endpoint"`
	// ProtocolVersion is the MCP protocol version spoken by this group endpoint.
	// The shell sends it on MCP requests so each group can be checked and
	// upgraded independently during protocol migrations.
	ProtocolVersion string `json:"protocolVersion"`
	AuthRequired    bool   `json:"authRequired"`
	// SkillURL points to the SKILL.md file whose frontmatter defines activation,
	// instructions, and the MCP tool subset for this skill.
	SkillURL string `json:"skillUrl,omitempty"`
}

// Validate returns an error if g is not a well-formed tool group manifest entry.
func (g *ToolGroup) Validate() error {
	return g.validate("toolGroup")
}

func (g *ToolGroup) validate(prefix string) error {
	var errs []error
	errs = append(errs,
		requireString(prefix+".name", g.Name),
		requireString(prefix+".endpoint", g.Endpoint),
		validateURLReference(prefix+".endpoint", g.Endpoint),
		requireString(prefix+".protocolVersion", g.ProtocolVersion),
	)
	if strings.TrimSpace(g.SkillURL) != "" {
		errs = append(errs, validateURLReference(prefix+".skillUrl", g.SkillURL))
	}
	return errors.Join(errs...)
}

// SkillFrontmatter is the YAML frontmatter schema for a Go Mode SKILL.md file.
//
// The Markdown body carries human and model instructions. The frontmatter carries
// machine-readable activation hints and the MCP tool allowlist that the native
// shell can register when the skill becomes active.
type SkillFrontmatter struct {
	Name        string      `json:"name"        yaml:"name"`
	Description string      `json:"description" yaml:"description"`
	GoMode      SkillGoMode `json:"gomode"      yaml:"gomode"`
}

// Validate returns an error if f is not a well-formed Go Mode skill frontmatter.
func (f *SkillFrontmatter) Validate() error {
	return errors.Join(
		requireString("name", f.Name),
		requireString("description", f.Description),
		f.GoMode.validate("gomode"),
	)
}

// SkillGoMode contains Go Mode-specific SKILL.md frontmatter fields.
type SkillGoMode struct {
	Activation SkillActivation  `json:"activation,omitzero" yaml:"activation,omitempty"`
	MCPServers []SkillMCPServer `json:"mcpServers"          yaml:"mcpServers"`
}

func (g *SkillGoMode) validate(prefix string) error {
	var errs []error
	errs = append(errs, g.Activation.validate(prefix+".activation"))
	if len(g.MCPServers) == 0 {
		errs = append(errs, fmt.Errorf("%s.mcpServers is required", prefix))
	}
	for i := range g.MCPServers {
		errs = append(errs, g.MCPServers[i].validate(fmt.Sprintf("%s.mcpServers[%d]", prefix, i)))
	}
	return errors.Join(errs...)
}

// SkillActivation carries hints the shell matches against current context to
// decide when to load a skill. Matching is on-device: the service never receives
// the user's context or location just because a skill was considered.
type SkillActivation struct {
	// Locations are local physical-location and Wi-Fi hints that make this skill
	// an activation candidate.
	Locations []LocationActivation `json:"locations,omitempty" yaml:"locations,omitempty"`
}

// IsZero reports whether activation hints are absent for json omitzero.
func (a SkillActivation) IsZero() bool {
	return len(a.Locations) == 0
}

func (a SkillActivation) validate(prefix string) error {
	errs := make([]error, 0, len(a.Locations))
	for i := range a.Locations {
		errs = append(errs, a.Locations[i].validate(fmt.Sprintf("%s.locations[%d]", prefix, i)))
	}
	return errors.Join(errs...)
}

// SkillMCPServer declares which tools from an MCP endpoint belong to one skill.
type SkillMCPServer struct {
	Name     string `json:"name"     yaml:"name"`
	Endpoint string `json:"endpoint" yaml:"endpoint"`
	// ProtocolVersion is the MCP protocol version spoken by this endpoint.
	ProtocolVersion string `json:"protocolVersion" yaml:"protocolVersion"`
	AuthRequired    bool   `json:"authRequired"    yaml:"authRequired"`
	// Tools is the explicit allowlist of MCP tool names activated by the skill.
	Tools []string `json:"tools" yaml:"tools"`
}

// Validate returns an error if s is not a well-formed skill MCP server entry.
func (s *SkillMCPServer) Validate() error {
	return s.validate("skillMCPServer")
}

func (s *SkillMCPServer) validate(prefix string) error {
	var errs []error
	errs = append(errs,
		requireString(prefix+".name", s.Name),
		requireString(prefix+".endpoint", s.Endpoint),
		validateURLReference(prefix+".endpoint", s.Endpoint),
		requireString(prefix+".protocolVersion", s.ProtocolVersion),
	)
	if len(s.Tools) == 0 {
		errs = append(errs, fmt.Errorf("%s.tools is required", prefix))
	}
	for i, tool := range s.Tools {
		errs = append(errs, requireString(fmt.Sprintf("%s.tools[%d]", prefix, i), tool))
	}
	return errors.Join(errs...)
}

// LocationActivation groups one location-based activation signal for a skill.
type LocationActivation struct {
	WiFi             LocationWiFi     `json:"wifi,omitzero"             yaml:"wifi,omitempty"`
	PhysicalPosition PhysicalPosition `json:"physicalPosition,omitzero" yaml:"physicalPosition,omitempty"`
}

// IsZero reports whether the location activation has no trigger.
func (l LocationActivation) IsZero() bool {
	return l.WiFi.IsZero() && l.PhysicalPosition.IsZero()
}

func (l LocationActivation) validate(prefix string) error {
	var errs []error
	if l.IsZero() {
		errs = append(errs, fmt.Errorf("%s requires wifi.ssids or physicalPosition", prefix))
	}
	errs = append(errs,
		l.WiFi.validate(prefix+".wifi"),
		l.PhysicalPosition.validate(prefix+".physicalPosition"),
	)
	return errors.Join(errs...)
}

// LocationWiFi matches any one of the configured Wi-Fi SSIDs.
type LocationWiFi struct {
	// SSIDs are Wi-Fi network names that can activate the skill.
	SSIDs []string `json:"ssids,omitempty,omitzero" yaml:"ssids,omitempty"`
}

// IsZero reports whether there are no Wi-Fi SSIDs.
func (w LocationWiFi) IsZero() bool {
	return len(w.SSIDs) == 0
}

func (w LocationWiFi) validate(prefix string) error {
	errs := make([]error, 0, len(w.SSIDs))
	for i, ssid := range w.SSIDs {
		errs = append(errs, requireString(fmt.Sprintf("%s.ssids[%d]", prefix, i), ssid))
	}
	return errors.Join(errs...)
}

// PhysicalPosition matches a named latitude and longitude within RadiusMeters.
type PhysicalPosition struct {
	// Name is a user-facing label for this physical position.
	Name      string  `json:"name"      yaml:"name"`
	Latitude  float64 `json:"latitude"  yaml:"latitude"`
	Longitude float64 `json:"longitude" yaml:"longitude"`
	// RadiusMeters is the activation radius around the latitude and longitude.
	RadiusMeters float64 `json:"radiusMeters" yaml:"radiusMeters"`
}

// IsZero reports whether no physical position is configured.
func (p PhysicalPosition) IsZero() bool {
	return strings.TrimSpace(p.Name) == "" && p.Latitude == 0 && p.Longitude == 0 && p.RadiusMeters == 0
}

func (p PhysicalPosition) validate(prefix string) error {
	if p.IsZero() {
		return nil
	}
	var errs []error
	errs = append(errs, requireString(prefix+".name", p.Name))
	if p.Latitude < -90 || p.Latitude > 90 {
		errs = append(errs, fmt.Errorf("%s.latitude must be between -90 and 90", prefix))
	}
	if p.Longitude < -180 || p.Longitude > 180 {
		errs = append(errs, fmt.Errorf("%s.longitude must be between -180 and 180", prefix))
	}
	if p.RadiusMeters <= 0 {
		errs = append(errs, fmt.Errorf("%s.radiusMeters must be positive", prefix))
	}
	return errors.Join(errs...)
}

// VoiceGatewaySettings describes the preferred voice gateway for this service.
type VoiceGatewaySettings struct {
	Required      bool   `json:"required"`
	URL           string `json:"url,omitempty"`
	AuthRequired  bool   `json:"authRequired,omitempty"`
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
}

// Validate returns an error if s is not a well-formed voice gateway manifest entry.
func (s *VoiceGatewaySettings) Validate() error {
	return s.validate("voiceGateway")
}

func (s *VoiceGatewaySettings) validate(prefix string) error {
	var errs []error
	if s.Required {
		errs = append(errs, requireString(prefix+".url", s.URL))
	}
	if s.AuthRequired && strings.TrimSpace(s.URL) == "" {
		errs = append(errs, fmt.Errorf("%s.url is required when %s.authRequired is true", prefix, prefix))
	}
	if strings.TrimSpace(s.URL) != "" {
		errs = append(errs, validateURLReference(prefix+".url", s.URL))
	}
	if strings.TrimSpace(s.TokenEndpoint) != "" {
		errs = append(errs, validateURLReference(prefix+".tokenEndpoint", s.TokenEndpoint))
	}
	return errors.Join(errs...)
}

func requireString(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func requirePositive(name string, value int) error {
	if value <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	return nil
}

func validateURLReference(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not contain leading or trailing whitespace", name)
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL reference: %q", name, value)
	}
	if u.IsAbs() {
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%s must use http:// or https://, got %q", name, value)
		}
		if u.Host == "" {
			return fmt.Errorf("%s must include a host, got %q", name, value)
		}
		return nil
	}
	if strings.HasPrefix(value, "//") {
		return fmt.Errorf("%s must use http:// or https:// or an absolute path, got %q", name, value)
	}
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s must be an absolute URL or absolute path, got %q", name, value)
	}
	return nil
}
