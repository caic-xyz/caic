// Package apispec exposes shared API SDK generation specification types.
package apispec

import "reflect"

// ClientErrorModel describes how generated clients extract API error details.
type ClientErrorModel struct {
	TypeName string

	TSCodeExpr    string
	TSMessageExpr string
	TSDetailsExpr string

	KTCodeExpr    string
	KTMessageExpr string
	KTDetailsExpr string

	SwiftCodeExpr    string
	SwiftMessageExpr string
	SwiftDetailsExpr string
}

// Config holds configuration for SDK code generation.
type Config struct {
	Routes             []Route
	SDKPackagePaths    map[string]struct{}
	ExtraSeeds         []reflect.Type
	DocumentExtraSeeds bool

	ErrorModel         ClientErrorModel
	KotlinPackage      string
	APIDocTitle        string
	APIDocIntro        string
	ErrorDoc           string
	MCPProtocolVersion string
	SectionComments    map[string]string
	SpecialTypes       []SpecialType
	Discriminated      []string // Type names using kind-based dispatch in TS validators
	ErrorCodes         []ErrorCode
}

// ErrorCode describes an API error code for docs and generated constants.
type ErrorCode struct {
	Code   string
	Status int
}

// Route describes a single API endpoint for generated clients and docs.
type Route struct {
	Name        string
	Doc         string
	Method      string
	Path        string
	Category    string
	Req         reflect.Type
	Resp        reflect.Type
	IsArray     bool
	IsSSE       bool
	SSEEvents   []SSEEvent
	QueryParams []string
	HeadersArg  bool
}

// SSEEvent describes a named event emitted in addition to an SSE route's
// primary message event. A nil Resp denotes a payloadless notification.
type SSEEvent struct {
	Name    string
	Handler string
	Resp    reflect.Type
}

// SpecialType defines custom mappings for Go types that need non-standard serialization.
type SpecialType struct {
	Type       reflect.Type
	TSType     string // TypeScript type
	KTType     string // Kotlin type
	SwiftType  string // Swift type
	DocType    string // API doc type
	TSValidate string // TS validator format string: %[1]s=pathExpr, %[2]q=pathLit
}
