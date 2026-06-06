// SDK generation configuration and configured type helpers.

package main

import (
	"fmt"
	"reflect"
)

// errorCodeDef describes an API error code for docs and generated constants.
type errorCodeDef struct {
	code   string
	status int
}

// genConfig holds configuration for SDK code generation, created once in mainImpl
// and threaded through docRegistry.
type genConfig struct {
	routes          []routeDef
	sdkPackagePaths map[string]struct{}
	extraSeeds      []reflect.Type

	errorModel      clientErrorModel
	kotlinPackage   string
	apiDocTitle     string
	apiDocIntro     string
	sectionComments map[string]string
	specialTypes    []specialType
	sseSeeds        []reflect.Type
	discriminated   []string // Type names using kind-based dispatch in TS validators
	errorCodes      []errorCodeDef
}

// lookupSpecial returns the matching specialType entry for t, or nil.
func (c *genConfig) lookupSpecial(t reflect.Type) *specialType {
	for i := range c.specialTypes {
		if c.specialTypes[i].t == t {
			return &c.specialTypes[i]
		}
	}
	return nil
}

func (c *genConfig) isSDKPkg(pkgPath string) bool {
	_, ok := c.sdkPackagePaths[pkgPath]
	return ok
}

func (c *genConfig) clientErrorModel() clientErrorModel {
	m := c.errorModel
	if m.typeName == "" {
		m.typeName = "ErrorResponse"
	}
	if m.tsCodeExpr == "" {
		m.tsCodeExpr = "err.error.code"
	}
	if m.tsMessageExpr == "" {
		m.tsMessageExpr = "err.error.message"
	}
	if m.tsDetailsExpr == "" {
		m.tsDetailsExpr = "err.details"
	}
	if m.ktCodeExpr == "" {
		m.ktCodeExpr = "err.error.code"
	}
	if m.ktMessageExpr == "" {
		m.ktMessageExpr = "err.error.message"
	}
	if m.ktDetailsExpr == "" {
		m.ktDetailsExpr = "err.details"
	}
	if m.swiftCodeExpr == "" {
		m.swiftCodeExpr = "errResp.error.code"
	}
	if m.swiftMessageExpr == "" {
		m.swiftMessageExpr = "errResp.error.message"
	}
	if m.swiftDetailsExpr == "" {
		m.swiftDetailsExpr = "nil"
	}
	return m
}

type clientErrorModel struct {
	typeName string

	tsCodeExpr    string
	tsMessageExpr string
	tsDetailsExpr string

	ktCodeExpr    string
	ktMessageExpr string
	ktDetailsExpr string

	swiftCodeExpr    string
	swiftMessageExpr string
	swiftDetailsExpr string
}

// specialTypes defines custom mappings for Go types that need non-standard
// serialization in the generated SDK clients. Each entry maps a Go reflect.Type
// to its language-specific representation and optional TS validator template.
type specialType struct {
	t          reflect.Type
	tsType     string // TypeScript type
	ktType     string // Kotlin type
	swiftType  string // Swift type
	docType    string // API doc type
	tsValidate string // TS validator format string: %[1]s=pathExpr, %[2]q=pathLit
}

// walkSDKTypes traverses struct types reachable from seeds in post-order
// (leaves first), returning only configured SDK package struct types.
func (c *genConfig) walkSDKTypes(seeds []reflect.Type) []reflect.Type {
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
func (c *genConfig) collectNamedSlices(structs []sdkType) []reflect.Type {
	seen := map[reflect.Type]struct{}{}
	var order []reflect.Type
	for _, ks := range structs {
		for f := range ks.t.Fields() {
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
func (c *genConfig) routeSeedTypes() []reflect.Type {
	var seeds []reflect.Type
	for i := range c.routes {
		r := &c.routes[i]
		if r.Req != nil {
			seeds = append(seeds, r.Req)
		}
		seeds = append(seeds, r.Resp)
	}
	return seeds
}

func (c *genConfig) sdkSeedTypes() []reflect.Type {
	seeds := c.routeSeedTypes()
	seeds = append(seeds, c.extraSeeds...)
	return seeds
}

// tsPrimitiveValidator returns the validator expression for a primitive/special type.
// For special types with a tsValidate template, it formats the expression using
// pathExpr and pathLit. Otherwise it falls back to kind-based validation.
func (c *genConfig) tsPrimitiveValidator(t reflect.Type, pathExpr, pathLit string) (string, error) {
	if m := c.lookupSpecial(t); m != nil && m.tsValidate != "" {
		return fmt.Sprintf(m.tsValidate, pathExpr, pathLit), nil
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
func (c *genConfig) tsElemValidatorFunc(t reflect.Type, pathLit string) (string, error) {
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
func (c *genConfig) goTypeToDoc(t reflect.Type) (string, error) {
	if t.Kind() == reflect.Pointer {
		return c.goTypeToDoc(t.Elem())
	}
	if m := c.lookupSpecial(t); m != nil {
		return m.docType, nil
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
		return "Record<string, unknown>", nil
	default:
		return "", fmt.Errorf("goTypeToDoc: unhandled kind %s for %s", t.Kind(), t)
	}
}
