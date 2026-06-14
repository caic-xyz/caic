// SDK generation specification for the voice gateway API.

package v1

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/apisdkgen/apispec"
	voiceapi "github.com/caic-xyz/caic/backend/internal/voicegateway/api"
)

// SDKAPI returns the SDK generation specification for the voice gateway API.
func SDKAPI() apispec.Config {
	return apispec.Config{
		Routes: sdkRoutes(),
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[StatusResp]().PkgPath():             {},
			reflect.TypeFor[voiceapi.ErrorResponse]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			reflect.TypeFor[voiceapi.ErrorResponse](),
			reflect.TypeFor[MessageEnvelope](),
			reflect.TypeFor[SessionSetup](),
			reflect.TypeFor[ContextUpdate](),
			reflect.TypeFor[UserMessage](),
			reflect.TypeFor[ToolResult](),
			reflect.TypeFor[TurnCancel](),
			reflect.TypeFor[SessionClose](),
			reflect.TypeFor[SessionReady](),
			reflect.TypeFor[TranscriptDelta](),
			reflect.TypeFor[AssistantTextDelta](),
			reflect.TypeFor[SpeechStarted](),
			reflect.TypeFor[SpeechEnded](),
			reflect.TypeFor[ToolCall](),
			reflect.TypeFor[Interrupted](),
			reflect.TypeFor[Error](),
		},
		DocumentExtraSeeds: true,
		KotlinPackage:      "com.caic.voicegateway.sdk.v1",
		APIDocTitle:        "Voice Gateway API Reference",
		APIDocIntro:        "RESTful JSON signaling API served at `/api/voicegateway/v1/voice/`.",
		SpecialTypes: []apispec.SpecialType{
			{
				Type:       reflect.TypeFor[json.RawMessage](),
				TSType:     "any /* json.RawMessage */",
				KTType:     "JsonElement",
				SwiftType:  "JSONValue",
				DocType:    "JSONValue",
				TSValidate: "%[1]s",
			},
			{
				Type:      reflect.TypeFor[map[string]any](),
				TSType:    "{ [key: string]: any /* json.RawMessage */}",
				KTType:    "Map<String, JsonElement>",
				SwiftType: "[String: JSONValue]",
				DocType:   "Record<string, JSONValue>",
			},
		},
		ErrorCodes: []apispec.ErrorCode{
			{Code: "BAD_REQUEST", Status: 400},
			{Code: "UNAUTHORIZED", Status: 401},
			{Code: "INTERNAL_ERROR", Status: 500},
		},
	}
}

func sdkRoutes() []apispec.Route {
	routes := make([]apispec.Route, len(Routes))
	for i := range Routes {
		r := &Routes[i]
		routes[i] = apispec.Route{
			Name:        r.Name,
			Doc:         r.Doc,
			Method:      r.Method,
			Path:        r.Path,
			Category:    r.CategoryName(),
			Req:         r.Req,
			Resp:        r.Resp,
			IsArray:     r.IsArray,
			IsSSE:       r.IsSSE,
			QueryParams: slices.Clone(r.QueryParams),
			HeadersArg:  true,
		}
	}
	return routes
}
