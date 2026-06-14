// SDK generation configuration and configured type helpers.

package apisdkgen

import (
	"fmt"
	"reflect"
)

// ErrorCode describes an API error code for docs and generated constants.
type ErrorCode struct {
	Code   string
	Status int
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
	SSESeeds           []reflect.Type
	Discriminated      []string // Type names using kind-based dispatch in TS validators
	ErrorCodes         []ErrorCode
}

// lookupSpecial returns the matching SpecialType entry for t, or nil.
func (c *Config) lookupSpecial(t reflect.Type) *SpecialType {
	for i := range c.SpecialTypes {
		if c.SpecialTypes[i].Type == t {
			return &c.SpecialTypes[i]
		}
	}
	return nil
}

func (c *Config) isSDKPkg(pkgPath string) bool {
	_, ok := c.SDKPackagePaths[pkgPath]
	return ok
}

func (c *Config) clientErrorModel() ClientErrorModel {
	m := c.ErrorModel
	if m.TypeName == "" {
		m.TypeName = "ErrorResponse"
	}
	if m.TSCodeExpr == "" {
		m.TSCodeExpr = "err.error.code"
	}
	if m.TSMessageExpr == "" {
		m.TSMessageExpr = "err.error.message"
	}
	if m.TSDetailsExpr == "" {
		m.TSDetailsExpr = "err.details"
	}
	if m.KTCodeExpr == "" {
		m.KTCodeExpr = "err.error.code"
	}
	if m.KTMessageExpr == "" {
		m.KTMessageExpr = "err.error.message"
	}
	if m.KTDetailsExpr == "" {
		m.KTDetailsExpr = "err.details"
	}
	if m.SwiftCodeExpr == "" {
		m.SwiftCodeExpr = "errResp.error.code"
	}
	if m.SwiftMessageExpr == "" {
		m.SwiftMessageExpr = "errResp.error.message"
	}
	if m.SwiftDetailsExpr == "" {
		m.SwiftDetailsExpr = "nil"
	}
	return m
}

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

// SpecialType defines custom mappings for Go types that need non-standard
// serialization in the generated SDK clients. Each entry maps a Go reflect.Type
// to its language-specific representation and optional TS validator template.
type SpecialType struct {
	Type       reflect.Type
	TSType     string // TypeScript type
	KTType     string // Kotlin type
	SwiftType  string // Swift type
	DocType    string // API doc type
	TSValidate string // TS validator format string: %[1]s=pathExpr, %[2]q=pathLit
}

// walkSDKTypes traverses struct types reachable from seeds in post-order
// (leaves first), returning only configured SDK package struct types.
func (c *Config) walkSDKTypes(seeds []reflect.Type) []reflect.Type {
	seen := map[reflect.Type]struct{}{}
	var order []reflect.Type

	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() == reflect.Slice {
			walk(t.Elem())
			return
		}
		if t.Kind() == reflect.Map {
			walk(t.Elem())
			return
		}
		if t.Kind() != reflect.Struct || !c.isSDKPkg(t.PkgPath()) {
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		for f := range t.Fields() {
			walk(f.Type)
		}
		order = append(order, t)
	}

	for _, t := range seeds {
		walk(t)
	}
	return order
}

// collectNamedSlices returns named slice types from SDK packages that appear
// as fields in the given struct types.
func (c *Config) collectNamedSlices(structs []sdkType) []reflect.Type {
	seen := map[reflect.Type]struct{}{}
	var order []reflect.Type
	for _, ks := range structs {
		for f := range ks.Type.Fields() {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() != reflect.Slice || !c.isSDKPkg(ft.PkgPath()) {
				continue
			}
			if _, ok := seen[ft]; ok {
				continue
			}
			seen[ft] = struct{}{}
			order = append(order, ft)
		}
	}
	return order
}

// routeSeedTypes returns the set of request and response types from the
// route table, used to seed type discovery for SDK generation.
func (c *Config) routeSeedTypes() []reflect.Type {
	var seeds []reflect.Type
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Req != nil {
			seeds = append(seeds, r.Req)
		}
		seeds = append(seeds, r.Resp)
	}
	return seeds
}

func (c *Config) hasSSERoutes() bool {
	for i := range c.Routes {
		if c.Routes[i].IsSSE {
			return true
		}
	}
	return false
}

func (c *Config) sdkSeedTypes() []reflect.Type {
	seeds := c.routeSeedTypes()
	seeds = append(seeds, c.ExtraSeeds...)
	return seeds
}

// tsPrimitiveValidator returns the validator expression for a primitive/special type.
// For special types with a tsValidate template, it formats the expression using
// pathExpr and pathLit. Otherwise it falls back to kind-based validation.
func (c *Config) tsPrimitiveValidator(t reflect.Type, pathExpr, pathLit string) (string, error) {
	if m := c.lookupSpecial(t); m != nil && m.TSValidate != "" {
		return fmt.Sprintf(m.TSValidate, pathExpr, pathLit), nil
	}
	switch t.Kind() {
	case reflect.String:
		return fmt.Sprintf("asString(%s, %q)", pathExpr, pathLit), nil
	case reflect.Int, reflect.Int64, reflect.Uint64, reflect.Float64:
		return fmt.Sprintf("asNumber(%s, %q)", pathExpr, pathLit), nil
	case reflect.Bool:
		return fmt.Sprintf("asBoolean(%s, %q)", pathExpr, pathLit), nil
	default:
		return "", fmt.Errorf("tsPrimitiveValidator: unhandled kind %s for %s", t.Kind(), t)
	}
}

// tsElemValidatorFunc returns a function expression (v: unknown) => validated
// for use as the element validator callback in validateArray/validateRecord.
func (c *Config) tsElemValidatorFunc(t reflect.Type, pathLit string) (string, error) {
	if t.Kind() == reflect.Pointer {
		return c.tsElemValidatorFunc(t.Elem(), pathLit)
	}
	if t.Kind() == reflect.Struct && c.isSDKPkg(t.PkgPath()) {
		return "validate" + t.Name(), nil
	}
	v, err := c.tsPrimitiveValidator(t, "v", pathLit)
	if err != nil {
		return "", err
	}
	return "(v) => " + v, nil
}

// goTypeToDoc maps a Go reflect.Type to a TypeScript-style string for docs.
func (c *Config) goTypeToDoc(t reflect.Type) (string, error) {
	if t.Kind() == reflect.Pointer {
		return c.goTypeToDoc(t.Elem())
	}
	if m := c.lookupSpecial(t); m != nil {
		return m.DocType, nil
	}
	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Int:
		return "int", nil
	case reflect.Int64:
		return "int64", nil
	case reflect.Uint64:
		return "uint64", nil
	case reflect.Float64:
		return "float64", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Slice:
		if c.isSDKPkg(t.PkgPath()) {
			return t.Name(), nil // Named slice.
		}
		e, err := c.goTypeToDoc(t.Elem())
		if err != nil {
			return "", err
		}
		return e + "[]", nil
	case reflect.Struct:
		return t.Name(), nil
	case reflect.Map:
		k, err := c.goTypeToDoc(t.Key())
		if err != nil {
			return "", err
		}
		v, err := c.goTypeToDoc(t.Elem())
		if err != nil {
			return "", err
		}
		return "Record<" + k + ", " + v + ">", nil
	default:
		return "", fmt.Errorf("goTypeToDoc: unhandled kind %s for %s", t.Kind(), t)
	}
}
