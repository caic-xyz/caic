// SDK generation specification for the caic API.

package v1

import (
	"encoding/json"
	"reflect"
	"slices"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/maruel/apisdkgen/apispec"
)

// SDKAPI returns the SDK generation specification for the caic API.
func SDKAPI() apispec.Config[api.ErrorCode] {
	return apispec.Config[api.ErrorCode]{
		Routes: sdkRoutes(),
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[StatusResp]().PkgPath():        {},
			reflect.TypeFor[api.ErrorResponse]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			reflect.TypeFor[api.ErrorResponse](),
			reflect.TypeFor[AuthBootstrapResp](),
		},
		KotlinPackage: "com.caic.sdk.v1",
		APIDocTitle:   "caic API Reference",
		APIDocIntro:   "RESTful JSON API served at `/api/caic/v1/`. SSE endpoints stream newline-delimited JSON events.\n\nThe read-only MCP tool `get_system_stats` takes `{}` and returns `SystemStatsResp`, the same snapshot as `GET /api/caic/v1/server/system-stats` (SDK method `getSystemStats`). It is available to `caic:mcp.read` clients and task-scoped agents, without a browser or UI. `samples` contains up to 60 five-second observations, oldest first; the array is empty until the first successful collection. Optional `error` reports a sampling warning while retained samples remain readable. Each `cpus` entry has a stable logical CPU ID and `usagePerc` from 0–100%. Its array length is the online CPU count; sum the percentages for total usage (100% per fully busy CPU), or divide that sum by 100 for CPU equivalents. Task `EventStats.cpuPerc` uses the same total-usage convention; `cpuCeiling` is the configured upper bound in fractional CPU units, derived from the owning runtime logical CPU count, explicit quota, and configured CPU set; null means unknown. Inherited cgroup limits and process affinity are unreported, so effective task capacity is unknown. `runtimeCPUs` is the owning runtime logical CPU count, including remote/VM runtimes; null means unknown. `cpuQuota` preserves the explicit fractional quota: zero means no explicit quota, null means unknown. `cpuError` reports runtime metadata observation failures while usage remains readable. CPU configuration refreshes every 30 seconds during active streams; task aggregate stats do not reveal per-CPU distribution. Memory excludes available/reclaimable pages. Memory, network counters, and filesystem capacities use bytes. Network sums all interfaces and can count traffic on multiple interfaces; disk covers the server working-directory filesystem.",
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
		// TaskListEvent has optional snapshot metadata as well as kind payloads.
		// Validate all present fields; kind-only dispatch would discard complete.
		Discriminated: []string{"EventMessage"},
		ErrorCodes: []apispec.ErrorCodeSpec[api.ErrorCode]{
			{Code: api.CodeBadRequest, Status: 400},
			{Code: api.CodeUnknownHarness, Status: 400},
			{Code: api.CodeUnknownRepository, Status: 400},
			{Code: api.CodeUnsupportedModel, Status: 400},
			{Code: api.CodeInvalidOAuthState, Status: 400},
			{Code: api.CodeUnknownCache, Status: 400},
			{Code: api.CodeUnknownRuntime, Status: 400},
			{Code: api.CodeUnauthorized, Status: 401},
			{Code: api.CodeForbidden, Status: 403},
			{Code: api.CodeNotFound, Status: 404},
			{Code: api.CodeOAuthGrantNotFound, Status: 404},
			{Code: api.CodeOAuthProviderUnavailable, Status: 404},
			{Code: api.CodeConflict, Status: 409},
			{Code: api.CodeRepositoryPathConflict, Status: 409},
			{Code: api.CodeInternalError, Status: 500},
			{Code: api.CodeUpdateCheckFailed, Status: 500},
			{Code: api.CodeUpdateUnavailable, Status: 500},
		},
	}
}

func sdkRoutes() []apispec.Route {
	routes := make([]apispec.Route, len(Routes))
	for i := range Routes {
		r := &Routes[i]
		namedEvents := make([]apispec.SSEEvent, len(r.SSEEvents))
		for j := range r.SSEEvents {
			namedEvents[j] = apispec.SSEEvent{
				Name:    r.SSEEvents[j].Name,
				Handler: r.SSEEvents[j].Handler,
				Resp:    r.SSEEvents[j].Resp,
			}
		}
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
			SSEEvents:   namedEvents,
			QueryParams: slices.Clone(r.QueryParams),
		}
	}
	return routes
}
