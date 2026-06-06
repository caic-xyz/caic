// Project-specific entry point and configuration for caic API SDK generation.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
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
	return generateVoiceGatewaySDK()
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
		apiDocIntro:   "RESTful JSON API served at `/api/v1/`. SSE endpoints stream newline-delimited JSON events.",
		sectionComments: map[string]string{
			"EventMessage": "Backend-neutral event types",
		},
		specialTypes: []specialType{
			{
				t:          reflect.TypeFor[json.RawMessage](),
				tsType:     "any /* json.RawMessage */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "object",
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
				docType:   "Record<string, unknown>",
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
		sourceDir = "../../../voicegateway"
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
			reflect.TypeFor[voicegateway.Compatibility]().PkgPath(): {},
		},
		extraSeeds: []reflect.Type{
			reflect.TypeFor[voicegateway.ErrorResponse](),
		},
		kotlinPackage: "com.caic.voicegateway.sdk.v1",
		apiDocTitle:   "Voice Gateway API Reference",
		apiDocIntro:   "RESTful JSON signaling API served by the standalone voice gateway.",
		specialTypes: []specialType{
			{
				t:          reflect.TypeFor[json.RawMessage](),
				tsType:     "any /* json.RawMessage */",
				ktType:     "JsonElement",
				swiftType:  "JSONValue",
				docType:    "object",
				tsValidate: "%[1]s",
			},
			{
				t:         reflect.TypeFor[map[string]any](),
				tsType:    "{ [key: string]: any /* json.RawMessage */}",
				ktType:    "Map<String, JsonElement>",
				swiftType: "[String: JSONValue]",
				docType:   "Record<string, unknown>",
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
	return []routeDef{
		{
			Name:     "health",
			Doc:      "Returns gateway health status.",
			Method:   "GET",
			Path:     "/health",
			Category: "Health",
			Resp:     reflect.TypeFor[voicegateway.HealthResp](),
		},
		{
			Name:     "compat",
			Doc:      "Returns gateway compatibility metadata.",
			Method:   "GET",
			Path:     "/compat",
			Category: "Compatibility",
			Resp:     reflect.TypeFor[voicegateway.Compatibility](),
		},
		{
			Name:     "offer",
			Doc:      "Creates a WebRTC voice session from an SDP offer.",
			Method:   "POST",
			Path:     "/offer",
			Category: "Sessions",
			Req:      reflect.TypeFor[voicegateway.OfferReq](),
			Resp:     reflect.TypeFor[voicegateway.OfferResp](),
		},
		{
			Name:     "closeSession",
			Doc:      "Closes an active voice gateway session.",
			Method:   "POST",
			Path:     "/sessions/{sessionID}",
			Category: "Sessions",
			Resp:     reflect.TypeFor[voicegateway.CloseSessionResp](),
		},
	}
}
