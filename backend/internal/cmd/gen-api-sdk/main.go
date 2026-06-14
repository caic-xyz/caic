// Project-specific entry point and configuration for caic API SDK generation.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/apisdkgen"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	voiceapi "github.com/caic-xyz/caic/backend/internal/voicegateway/api"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-api-sdk: %v\n", err)
		os.Exit(1)
	}
}

func mainImpl() error {
	if err := generateCaicSDK(); err != nil {
		return err
	}
	if err := generateVoiceGatewaySDK(); err != nil {
		return err
	}
	if err := generateMcpSDK(); err != nil {
		return err
	}
	return generateGoModeSDK()
}

func generateCaicSDK() error {
	// Directories are relative to go:generate CWD (backend/internal/server/api/v1/).
	const (
		sdkDir    = "../../../../../sdk/caic"
		tsDir     = sdkDir + "/ts/v1"
		kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/caic/sdk/v1"
		swiftDir  = sdkDir + "/swift/Sources/CaicSDK"
	)
	return apisdkgen.Generate(&apisdkgen.API{
		SourceDir: ".",
		Output: apisdkgen.OutputConfig{
			TypeScriptDir: tsDir,
			KotlinDir:     kotlinDir,
			SwiftDir:      swiftDir,
			MarkdownDir:   sdkDir,
		},
		Config: apisdkgen.Config{
			Routes: caicRoutes(),
			SDKPackagePaths: map[string]struct{}{
				reflect.TypeFor[v1.StatusResp]().PkgPath():     {},
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
			SpecialTypes: []apisdkgen.SpecialType{
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
				reflect.TypeFor[v1.EventMessage](),
				reflect.TypeFor[v1.TaskListEvent](),
				reflect.TypeFor[v1.UsageResp](),
			},
			Discriminated: []string{"EventMessage", "TaskListEvent"},
			ErrorCodes: []apisdkgen.ErrorCode{
				{Code: string(api.CodeBadRequest), Status: 400},
				{Code: string(api.CodeNotFound), Status: 404},
				{Code: string(api.CodeConflict), Status: 409},
				{Code: string(api.CodeInternalError), Status: 500},
			},
		},
	})
}

func generateVoiceGatewaySDK() error {
	// Directories are relative to go:generate CWD (backend/internal/server/api/v1/).
	const (
		sourceDir = "../../../voicegateway/api/v1"
		sdkDir    = "../../../../../sdk/voicegateway"
		tsDir     = sdkDir + "/ts/v1"
		kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/caic/voicegateway/sdk/v1"
		swiftDir  = sdkDir + "/swift/Sources/VoiceGatewaySDK"
	)
	return apisdkgen.Generate(&apisdkgen.API{
		SourceDir: sourceDir,
		Output: apisdkgen.OutputConfig{
			TypeScriptDir: tsDir,
			KotlinDir:     kotlinDir,
			SwiftDir:      swiftDir,
			MarkdownDir:   sdkDir,
		},
		Config: apisdkgen.Config{
			Routes: voiceGatewayRoutes(),
			SDKPackagePaths: map[string]struct{}{
				reflect.TypeFor[voicev1.StatusResp]().PkgPath():     {},
				reflect.TypeFor[voiceapi.ErrorResponse]().PkgPath(): {},
			},
			ExtraSeeds: []reflect.Type{
				reflect.TypeFor[voiceapi.ErrorResponse](),
				reflect.TypeFor[voicev1.MessageEnvelope](),
				reflect.TypeFor[voicev1.SessionSetup](),
				reflect.TypeFor[voicev1.ContextUpdate](),
				reflect.TypeFor[voicev1.UserMessage](),
				reflect.TypeFor[voicev1.ToolResult](),
				reflect.TypeFor[voicev1.TurnCancel](),
				reflect.TypeFor[voicev1.SessionClose](),
				reflect.TypeFor[voicev1.SessionReady](),
				reflect.TypeFor[voicev1.TranscriptDelta](),
				reflect.TypeFor[voicev1.AssistantTextDelta](),
				reflect.TypeFor[voicev1.SpeechStarted](),
				reflect.TypeFor[voicev1.SpeechEnded](),
				reflect.TypeFor[voicev1.ToolCall](),
				reflect.TypeFor[voicev1.Interrupted](),
				reflect.TypeFor[voicev1.Error](),
			},
			DocumentExtraSeeds: true,
			KotlinPackage:      "com.caic.voicegateway.sdk.v1",
			APIDocTitle:        "Voice Gateway API Reference",
			APIDocIntro:        "RESTful JSON signaling API served at `/api/voicegateway/v1/voice/`.",
			SpecialTypes: []apisdkgen.SpecialType{
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
			ErrorCodes: []apisdkgen.ErrorCode{
				{Code: "BAD_REQUEST", Status: 400},
				{Code: "UNAUTHORIZED", Status: 401},
				{Code: "INTERNAL_ERROR", Status: 500},
			},
		},
	})
}

func caicRoutes() []apisdkgen.Route {
	routes := make([]apisdkgen.Route, len(v1.Routes))
	for i := range v1.Routes {
		r := &v1.Routes[i]
		routes[i] = apisdkgen.Route{
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

func voiceGatewayRoutes() []apisdkgen.Route {
	routes := make([]apisdkgen.Route, len(voicev1.Routes))
	for i := range voicev1.Routes {
		r := &voicev1.Routes[i]
		routes[i] = apisdkgen.Route{
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

func generateMcpSDK() error {
	// Directories are relative to go:generate CWD (backend/internal/server/api/v1/).
	const (
		sourceDir = "../../../mcp"
		sdkDir    = "../../../../../sdk/mcp"
		tsDir     = sdkDir + "/ts/v1"
		kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/fghbuild/mcp/sdk/v1"
		swiftDir  = sdkDir + "/swift/Sources/MCPSDK"
	)
	return apisdkgen.Generate(&apisdkgen.API{
		SourceDir: sourceDir,
		Output: apisdkgen.OutputConfig{
			TypeScriptDir: tsDir,
			KotlinDir:     kotlinDir,
			SwiftDir:      swiftDir,
			MarkdownDir:   sdkDir,
		},
		Config: apisdkgen.Config{
			Routes: []apisdkgen.Route{
				{
					Name:       "mcp",
					Doc:        "Sends a raw MCP JSON-RPC request to the client base URL. The caller supplies required MCP transport headers.",
					Method:     "POST",
					Path:       "",
					Category:   "MCP",
					Req:        reflect.TypeFor[mcp.JSONRPCRequest](),
					Resp:       reflect.TypeFor[mcp.JSONRPCResponse](),
					HeadersArg: true,
				},
			},
			SDKPackagePaths: map[string]struct{}{
				reflect.TypeFor[mcp.JSONRPCRequest]().PkgPath(): {},
			},
			ExtraSeeds: []reflect.Type{
				reflect.TypeFor[mcp.ServerDiscoverResult](),
				reflect.TypeFor[mcp.PaginatedRequestParams](),
				reflect.TypeFor[mcp.ToolsListResult](),
				reflect.TypeFor[mcp.ToolsCallParams](),
				reflect.TypeFor[mcp.ToolCallResult](),
				reflect.TypeFor[mcp.ResourcesListResult](),
				reflect.TypeFor[mcp.ResourceTemplatesListResult](),
				reflect.TypeFor[mcp.ResourcesReadParams](),
				reflect.TypeFor[mcp.ResourcesReadResult](),
				reflect.TypeFor[mcp.SubscriptionsListenParams](),
				reflect.TypeFor[mcp.JSONRPCNotification](),
				reflect.TypeFor[mcp.SubscriptionNotificationParams](),
			},
			DocumentExtraSeeds: true,
			KotlinPackage:      "com.fghbuild.mcp.sdk.v1",
			APIDocTitle:        "MCP API Reference",
			APIDocIntro:        "JSON-RPC MCP endpoint client. Construct clients with the MCP endpoint URL advertised by the service. Requests must include MCP transport headers such as `Mcp-Protocol-Version` and `Mcp-Method`; method-specific requests may also require `Mcp-Name` and `Mcp-Param-*` headers.",
			ErrorDoc:           "MCP errors are JSON-RPC error objects in `JSONRPCResponse.error`. Transport-layer validation failures may use non-2xx HTTP statuses; method-level JSON-RPC errors can still use HTTP 200.",
			MCPProtocolVersion: mcp.ProtocolVersion,
			SpecialTypes: []apisdkgen.SpecialType{
				{
					Type:       reflect.TypeFor[json.RawMessage](),
					TSType:     "unknown /* json.RawMessage */",
					KTType:     "JsonElement",
					SwiftType:  "JSONValue",
					DocType:    "JSONValue",
					TSValidate: "%[1]s",
				},
				{
					Type:       reflect.TypeFor[any](),
					TSType:     "unknown",
					KTType:     "JsonElement",
					SwiftType:  "JSONValue",
					DocType:    "JSONValue",
					TSValidate: "%[1]s",
				},
				{
					Type:       reflect.TypeFor[jsonschema.Schema](),
					TSType:     "unknown /* JSON Schema */",
					KTType:     "JsonElement",
					SwiftType:  "JSONValue",
					DocType:    "JSONSchema",
					TSValidate: "%[1]s",
				},
			},
			ErrorModel: apisdkgen.ClientErrorModel{
				TypeName:         "JSONRPCResponse",
				TSCodeExpr:       "String(err.error?.code ?? \"UNKNOWN\")",
				TSMessageExpr:    "err.error?.message ?? \"\"",
				TSDetailsExpr:    "undefined",
				KTCodeExpr:       "err.error?.code?.toString() ?: \"UNKNOWN\"",
				KTMessageExpr:    "err.error?.message ?: \"\"",
				KTDetailsExpr:    "null",
				SwiftCodeExpr:    "String(errResp.error?.code ?? 0)",
				SwiftMessageExpr: "errResp.error?.message ?? \"\"",
				SwiftDetailsExpr: "nil",
			},
		},
	})
}

func generateGoModeSDK() error {
	// Directories are relative to go:generate CWD (backend/internal/server/api/v1/).
	const (
		sourceDir = "../../../gomode"
		sdkDir    = "../../../../../sdk/gomode"
		tsDir     = sdkDir + "/ts/v1"
		kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/fghbuild/gomode/sdk/v1"
		swiftDir  = sdkDir + "/swift/Sources/GoModeSDK"
	)
	return apisdkgen.Generate(&apisdkgen.API{
		SourceDir: sourceDir,
		Output: apisdkgen.OutputConfig{
			TypeScriptDir: tsDir,
			KotlinDir:     kotlinDir,
			SwiftDir:      swiftDir,
			MarkdownDir:   sdkDir,
		},
		Config: apisdkgen.Config{
			Routes: []apisdkgen.Route{
				{
					Name:     "getSettings",
					Doc:      "Returns Go Mode service compatibility settings.",
					Method:   "GET",
					Path:     "/api/gomode/v1/settings",
					Category: "Settings",
					Resp:     reflect.TypeFor[gomode.Settings](),
				},
			},
			SDKPackagePaths: map[string]struct{}{
				reflect.TypeFor[gomode.Settings]().PkgPath():      {},
				reflect.TypeFor[gomode.ErrorResponse]().PkgPath(): {},
			},
			ExtraSeeds: []reflect.Type{
				reflect.TypeFor[gomode.ErrorResponse](),
			},
			KotlinPackage: "com.fghbuild.gomode.sdk.v1",
			APIDocTitle:   "Go Mode Service Discovery API Reference",
			APIDocIntro:   "Service-neutral JSON API served at `/api/gomode/v1/` for Go Mode Android bootstrap.",
			SpecialTypes: []apisdkgen.SpecialType{
				{
					Type:      reflect.TypeFor[map[string]any](),
					TSType:    "{ [key: string]: any /* json.RawMessage */}",
					KTType:    "Map<String, JsonElement>",
					SwiftType: "[String: JSONValue]",
					DocType:   "Record<string, JSONValue>",
				},
			},
		},
	})
}
