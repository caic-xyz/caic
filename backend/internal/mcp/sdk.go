// SDK generation specification for the MCP API.

package mcp

import (
	"encoding/json"
	"reflect"

	"github.com/invopop/jsonschema"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
)

// SDKAPI returns the SDK generation specification for the MCP API.
func SDKAPI() apispec.Config {
	return apispec.Config{
		Routes: []apispec.Route{
			{
				Name:       "mcp",
				Doc:        "Sends a raw MCP JSON-RPC request to the client base URL. The caller supplies required MCP transport headers.",
				Method:     "POST",
				Path:       "",
				Category:   "MCP",
				Req:        reflect.TypeFor[JSONRPCRequest](),
				Resp:       reflect.TypeFor[JSONRPCResponse](),
				HeadersArg: true,
			},
		},
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[JSONRPCRequest]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			reflect.TypeFor[ServerDiscoverResult](),
			reflect.TypeFor[PaginatedRequestParams](),
			reflect.TypeFor[ToolsListResult](),
			reflect.TypeFor[ToolsCallParams](),
			reflect.TypeFor[ToolCallResult](),
			reflect.TypeFor[ResourcesListResult](),
			reflect.TypeFor[ResourceTemplatesListResult](),
			reflect.TypeFor[ResourcesReadParams](),
			reflect.TypeFor[ResourcesReadResult](),
			reflect.TypeFor[SubscriptionsListenParams](),
			reflect.TypeFor[JSONRPCNotification](),
			reflect.TypeFor[SubscriptionNotificationParams](),
		},
		DocumentExtraSeeds: true,
		KotlinPackage:      "com.fghbuild.mcp.sdk.v1",
		APIDocTitle:        "MCP API Reference",
		APIDocIntro:        "JSON-RPC MCP endpoint client. Construct clients with the MCP endpoint URL advertised by the service. Requests must include MCP transport headers such as `Mcp-Protocol-Version` and `Mcp-Method`; method-specific requests may also require `Mcp-Name` and `Mcp-Param-*` headers.",
		ErrorDoc:           "MCP errors are JSON-RPC error objects in `JSONRPCResponse.error`. Transport-layer validation failures may use non-2xx HTTP statuses; method-level JSON-RPC errors can still use HTTP 200.",
		MCPProtocolVersion: ProtocolVersion,
		SpecialTypes: []apispec.SpecialType{
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
		ErrorModel: apispec.ClientErrorModel{
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
	}
}
