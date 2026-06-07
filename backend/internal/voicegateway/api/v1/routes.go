// Voice gateway API route declarations used by the SDK generator.

package v1

import (
	"reflect"
	"strings"
)

// Route describes a single API endpoint for code generation.
type Route struct {
	Name        string       // Function name, e.g. "voiceRTCOffer"
	Doc         string       // One-line description for SDK comments and docs.
	Method      string       // HTTP method, e.g. "GET" or "POST"
	Path        string       // "/api/voicegateway/v1/voice/rtc/offer"
	Req         reflect.Type // Request body type; nil for no body.
	Resp        reflect.Type // Response body type.
	IsArray     bool         // response is T[] not T
	IsSSE       bool         // SSE stream, not JSON
	QueryParams []string     // Query parameter names (GET endpoints only).
}

// ReqName returns the request type name, or "" if Req is nil.
func (r *Route) ReqName() string {
	if r.Req == nil {
		return ""
	}
	return r.Req.Name()
}

// RespName returns the response type name.
func (r *Route) RespName() string {
	return r.Resp.Name()
}

// CategoryName returns the doc section derived from the first path segment
// after "/api/voicegateway/v1/", with the first letter uppercased.
func (r *Route) CategoryName() string {
	p := strings.TrimPrefix(r.Path, "/api/voicegateway/v1/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "Other"
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

// Routes is the authoritative list of voice gateway API endpoints.
var Routes = []Route{
	{
		Name:   "getVoiceToken",
		Doc:    "Returns a short-lived voice API token.",
		Method: "GET",
		Path:   "/api/voicegateway/v1/voice/token",
		Resp:   reflect.TypeFor[VoiceTokenResp](),
	},
	{
		Name:   "voiceRTCOffer",
		Doc:    "Exchanges a WebRTC SDP offer for an answer, opening a voice gateway session.",
		Method: "POST",
		Path:   "/api/voicegateway/v1/voice/rtc/offer",
		Req:    reflect.TypeFor[VoiceRTCOfferReq](),
		Resp:   reflect.TypeFor[VoiceRTCAnswerResp](),
	},
	{
		Name:   "closeVoiceRTC",
		Doc:    "Closes a WebRTC voice bridge session.",
		Method: "POST",
		Path:   "/api/voicegateway/v1/voice/rtc/{sessionID}",
		Resp:   reflect.TypeFor[StatusResp](),
	},
}
