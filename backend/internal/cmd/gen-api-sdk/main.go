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
		}
	}
	return routes
}
