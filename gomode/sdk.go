// SDK generation specification for the Go Mode service discovery API.

package gomode

import (
	"reflect"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
)

// SDKAPI returns the SDK generation specification for the Go Mode service discovery API.
func SDKAPI() apispec.Config {
	return apispec.Config{
		Routes: []apispec.Route{
			{
				Name:     "getSettings",
				Doc:      "Returns Go Mode service compatibility settings.",
				Method:   "GET",
				Path:     settingsPath,
				Category: "Settings",
				Resp:     reflect.TypeFor[Settings](),
			},
		},
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[Settings]().PkgPath():      {},
			reflect.TypeFor[ErrorResponse]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			reflect.TypeFor[ErrorResponse](),
		},
		KotlinPackage: "com.fghbuild.gomode.sdk.v1",
		APIDocTitle:   "Go Mode Service Discovery API Reference",
		APIDocIntro:   "Service-neutral JSON discovery manifest served publicly at `/.well-known/gomode.json` for Go Mode Android bootstrap.",
		SpecialTypes: []apispec.SpecialType{
			{
				Type:      reflect.TypeFor[map[string]any](),
				TSType:    "{ [key: string]: any /* json.RawMessage */}",
				KTType:    "Map<String, JsonElement>",
				SwiftType: "[String: JSONValue]",
				DocType:   "Record<string, JSONValue>",
			},
		},
	}
}
