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
	docs, err := loadDocsInDir(".")
	if err != nil {
		return fmt.Errorf("loading docs: %w", err)
	}
	// Code generation configuration.
	docs.cfg = &genConfig{
		routes: caicRoutes(),
		sdkPackagePaths: map[string]struct{}{
			reflect.TypeFor[v1.StatusResp]().PkgPath():     {},
			reflect.TypeFor[api.ErrorResponse]().PkgPath(): {},
		},
		extraSeeds: []reflect.Type{
			reflect.TypeFor[api.ErrorResponse](),
		},
		kotlinPackage: "com.caic.sdk.v1",
		apiDocTitle:   "caic API Reference",
		apiDocIntro:   "RESTful JSON API served at `/api/caic/v1/`. SSE endpoints stream newline-delimited JSON events.",
		sectionComments: map[string]string{
			"EventMessage": "Backend-neutral event types",
		},
		specialTypes: []specialType{
			{
				t:          reflect.TypeFor[json.RawMessage](),
				tsType:     "any /* json.RawMessage */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "JSONValue",
				tsValidate: "%[1]s",
			},
			{
				t:          reflect.TypeFor[ksid.ID](),
				tsType:     "string",
				ktType:     "String",
				swiftType:  "String",
				docType:    "string",
				tsValidate: "asString(%[1]s, %[2]q)",
			},
			{
				t:          reflect.TypeFor[time.Time](),
				tsType:     "ISOTimestamp",
				ktType:     "Instant",
				swiftType:  "ISOTimestamp",
				docType:    "ISOTimestamp",
				tsValidate: "asString(%[1]s, %[2]q) as ISOTimestamp",
			},
			{
				t:         reflect.TypeFor[map[string]any](),
				tsType:    "{ [key: string]: any /* json.RawMessage */}",
				ktType:    "Map<String, JsonElement>",
				swiftType: "[String: JSONValue]",
				docType:   "Record<string, JSONValue>",
			},
		},
		sseSeeds: []reflect.Type{
			reflect.TypeFor[v1.EventMessage](),
			reflect.TypeFor[v1.TaskListEvent](),
			reflect.TypeFor[v1.UsageResp](),
		},
		discriminated: []string{"EventMessage", "TaskListEvent"},
		errorCodes: []errorCodeDef{
			{string(api.CodeBadRequest), 400},
			{string(api.CodeNotFound), 404},
			{string(api.CodeConflict), 409},
			{string(api.CodeInternalError), 500},
		},
	}

	if err := docs.generateTSTypes(tsDir); err != nil {
		return err
	}
	if err := docs.generateTS(tsDir); err != nil {
		return err
	}
	if err := docs.generateTSValidate(tsDir); err != nil {
		return err
	}
	if err := docs.generateKotlin(kotlinDir); err != nil {
		return err
	}
	if err := docs.generateSwift(swiftDir); err != nil {
		return err
	}
	return docs.generateMarkdownDoc(sdkDir)
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
	docs, err := loadDocsInDir(sourceDir)
	if err != nil {
		return fmt.Errorf("loading voice gateway docs: %w", err)
	}
	docs.cfg = &genConfig{
		routes: voiceGatewayRoutes(),
		sdkPackagePaths: map[string]struct{}{
			reflect.TypeFor[voicev1.StatusResp]().PkgPath():     {},
			reflect.TypeFor[voiceapi.ErrorResponse]().PkgPath(): {},
		},
		extraSeeds: []reflect.Type{
			reflect.TypeFor[voiceapi.ErrorResponse](),
			reflect.TypeFor[voicev1.MessageEnvelope](),
			reflect.TypeFor[voicev1.SessionSetup](),
			reflect.TypeFor[voicev1.ContextUpdate](),
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
		documentExtraSeeds: true,
		kotlinPackage:      "com.caic.voicegateway.sdk.v1",
		apiDocTitle:        "Voice Gateway API Reference",
		apiDocIntro:        "RESTful JSON signaling API served at `/api/voicegateway/v1/voice/`.",
		specialTypes: []specialType{
			{
				t:          reflect.TypeFor[json.RawMessage](),
				tsType:     "any /* json.RawMessage */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "JSONValue",
				tsValidate: "%[1]s",
			},
			{
				t:         reflect.TypeFor[map[string]any](),
				tsType:    "{ [key: string]: any /* json.RawMessage */}",
				ktType:    "Map<String, JsonElement>",
				swiftType: "[String: JSONValue]",
				docType:   "Record<string, JSONValue>",
			},
		},
		errorCodes: []errorCodeDef{
			{"BAD_REQUEST", 400},
			{"UNAUTHORIZED", 401},
			{"INTERNAL_ERROR", 500},
		},
	}
	if err := docs.generateTSTypes(tsDir); err != nil {
		return err
	}
	if err := docs.generateTS(tsDir); err != nil {
		return err
	}
	if err := docs.generateTSValidate(tsDir); err != nil {
		return err
	}
	if err := docs.generateKotlin(kotlinDir); err != nil {
		return err
	}
	if err := docs.generateSwift(swiftDir); err != nil {
		return err
	}
	return docs.generateMarkdownDoc(sdkDir)
}

func caicRoutes() []routeDef {
	routes := make([]routeDef, len(v1.Routes))
	for i := range v1.Routes {
		r := &v1.Routes[i]
		routes[i] = routeDef{
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

func voiceGatewayRoutes() []routeDef {
	routes := make([]routeDef, len(voicev1.Routes))
	for i := range voicev1.Routes {
		r := &voicev1.Routes[i]
		routes[i] = routeDef{
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
	docs, err := loadDocsInDir(sourceDir)
	if err != nil {
		return fmt.Errorf("loading mcp docs: %w", err)
	}
	docs.cfg = &genConfig{
		routes: []routeDef{
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
		sdkPackagePaths: map[string]struct{}{
			reflect.TypeFor[mcp.JSONRPCRequest]().PkgPath(): {},
		},
		extraSeeds: []reflect.Type{
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
		documentExtraSeeds: true,
		kotlinPackage:      "com.fghbuild.mcp.sdk.v1",
		apiDocTitle:        "MCP API Reference",
		apiDocIntro:        "JSON-RPC MCP endpoint client. Construct clients with the MCP endpoint URL advertised by the service. Requests must include MCP transport headers such as `Mcp-Protocol-Version` and `Mcp-Method`; method-specific requests may also require `Mcp-Name` and `Mcp-Param-*` headers.",
		errorDoc:           "MCP errors are JSON-RPC error objects in `JSONRPCResponse.error`. Transport-layer validation failures may use non-2xx HTTP statuses; method-level JSON-RPC errors can still use HTTP 200.",
		specialTypes: []specialType{
			{
				t:          reflect.TypeFor[json.RawMessage](),
				tsType:     "unknown /* json.RawMessage */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "JSONValue",
				tsValidate: "%[1]s",
			},
			{
				t:          reflect.TypeFor[any](),
				tsType:     "unknown",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "JSONValue",
				tsValidate: "%[1]s",
			},
			{
				t:          reflect.TypeFor[jsonschema.Schema](),
				tsType:     "unknown /* JSON Schema */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "JSONSchema",
				tsValidate: "%[1]s",
			},
		},
		errorModel: clientErrorModel{
			typeName:         "JSONRPCResponse",
			tsCodeExpr:       "String(err.error?.code ?? \"UNKNOWN\")",
			tsMessageExpr:    "err.error?.message ?? \"\"",
			tsDetailsExpr:    "undefined",
			ktCodeExpr:       "err.error?.code?.toString() ?: \"UNKNOWN\"",
			ktMessageExpr:    "err.error?.message ?: \"\"",
			ktDetailsExpr:    "null",
			swiftCodeExpr:    "String(errResp.error?.code ?? 0)",
			swiftMessageExpr: "errResp.error?.message ?? \"\"",
			swiftDetailsExpr: "nil",
		},
	}
	if err := docs.generateTSTypes(tsDir); err != nil {
		return err
	}
	if err := docs.generateTS(tsDir); err != nil {
		return err
	}
	if err := docs.generateTSValidate(tsDir); err != nil {
		return err
	}
	if err := docs.generateKotlin(kotlinDir); err != nil {
		return err
	}
	if err := docs.generateSwift(swiftDir); err != nil {
		return err
	}
	return docs.generateMarkdownDoc(sdkDir)
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
	docs, err := loadDocsInDir(sourceDir)
	if err != nil {
		return fmt.Errorf("loading go mode docs: %w", err)
	}
	docs.cfg = &genConfig{
		routes: []routeDef{
			{
				Name:     "getSettings",
				Doc:      "Returns Go Mode service compatibility settings.",
				Method:   "GET",
				Path:     "/api/gomode/v1/settings",
				Category: "Settings",
				Resp:     reflect.TypeFor[gomode.Settings](),
			},
		},
		sdkPackagePaths: map[string]struct{}{
			reflect.TypeFor[gomode.Settings]().PkgPath():      {},
			reflect.TypeFor[gomode.ErrorResponse]().PkgPath(): {},
		},
		extraSeeds: []reflect.Type{
			reflect.TypeFor[gomode.ErrorResponse](),
		},
		kotlinPackage: "com.fghbuild.gomode.sdk.v1",
		apiDocTitle:   "Go Mode Service Discovery API Reference",
		apiDocIntro:   "Service-neutral JSON API served at `/api/gomode/v1/` for Go Mode Android bootstrap.",
		specialTypes: []specialType{
			{
				t:         reflect.TypeFor[map[string]any](),
				tsType:    "{ [key: string]: any /* json.RawMessage */}",
				ktType:    "Map<String, JsonElement>",
				swiftType: "[String: JSONValue]",
				docType:   "Record<string, JSONValue>",
			},
		},
	}
	if err := docs.generateTSTypes(tsDir); err != nil {
		return err
	}
	if err := docs.generateTS(tsDir); err != nil {
		return err
	}
	if err := docs.generateTSValidate(tsDir); err != nil {
		return err
	}
	if err := docs.generateKotlin(kotlinDir); err != nil {
		return err
	}
	if err := docs.generateSwift(swiftDir); err != nil {
		return err
	}
	return docs.generateMarkdownDoc(sdkDir)
}
