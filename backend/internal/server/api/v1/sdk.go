// SDK generation specification for the caic API.

package v1

import (
	"encoding/json"
	"reflect"
	"slices"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
	"github.com/caic-xyz/caic/backend/internal/server/api"
)

// SDKAPI returns the SDK generation specification for the caic API.
func SDKAPI() apispec.Config {
	return apispec.Config{
		Routes: sdkRoutes(),
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[StatusResp]().PkgPath():        {},
			reflect.TypeFor[api.ErrorResponse]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			reflect.TypeFor[api.ErrorResponse](),
		},
		KotlinPackage: "com.caic.sdk.v1",
		APIDocTitle:   "caic API Reference",
		APIDocIntro:   "RESTful JSON API served at `/api/caic/v1/`. SSE endpoints stream newline-delimited JSON events.",
		SectionComments: map[string]string{
			"EventMessage": "Backend-neutral event types",
		},
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
				Type:       reflect.TypeFor[ksid.ID](),
				TSType:     "string",
				KTType:     "String",
				SwiftType:  "String",
				DocType:    "string",
				TSValidate: "asString(%[1]s, %[2]q)",
			},
			{
				Type:       reflect.TypeFor[time.Time](),
				TSType:     "ISOTimestamp",
				KTType:     "Instant",
				SwiftType:  "ISOTimestamp",
				DocType:    "ISOTimestamp",
				TSValidate: "asString(%[1]s, %[2]q) as ISOTimestamp",
			},
			{
				Type:      reflect.TypeFor[map[string]any](),
				TSType:    "{ [key: string]: any /* json.RawMessage */}",
				KTType:    "Map<String, JsonElement>",
				SwiftType: "[String: JSONValue]",
				DocType:   "Record<string, JSONValue>",
			},
		},
		SSESeeds: []reflect.Type{
			reflect.TypeFor[EventMessage](),
			reflect.TypeFor[TaskListEvent](),
			reflect.TypeFor[UsageResp](),
		},
		Discriminated: []string{"EventMessage", "TaskListEvent"},
		ErrorCodes: []apispec.ErrorCode{
			{Code: string(api.CodeBadRequest), Status: 400},
			{Code: string(api.CodeNotFound), Status: 404},
			{Code: string(api.CodeConflict), Status: 409},
			{Code: string(api.CodeInternalError), Status: 500},
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
		}
	}
	return routes
}
