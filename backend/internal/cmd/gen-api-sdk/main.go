// Generates typed TypeScript, Kotlin, and Swift API clients plus API.md from the Go route declarations.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

var pathParamRe = regexp.MustCompile(`\{(\w+)\}`)

// errorCodeDef describes an API error code for docs and generated constants.
type errorCodeDef struct {
	code   api.ErrorCode
	status int
}

// genConfig holds configuration for SDK code generation, created once in mainImpl
// and threaded through docRegistry.
type genConfig struct {
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

// swiftReservedWords is the set of Swift keywords that require backtick escaping
// when used as property names.
var swiftReservedWords = map[string]struct{}{
	"init": {}, "deinit": {}, "class": {}, "struct": {}, "enum": {},
	"extension": {}, "protocol": {}, "var": {}, "let": {}, "func": {},
	"return": {}, "if": {}, "else": {}, "switch": {}, "case": {},
	"default": {}, "for": {}, "in": {}, "while": {}, "repeat": {},
	"do": {}, "try": {}, "catch": {}, "throw": {}, "throws": {},
	"import": {}, "typealias": {}, "where": {}, "guard": {},
	"defer": {}, "break": {}, "continue": {}, "fallthrough": {},
	"as": {}, "is": {}, "nil": {}, "true": {}, "false": {},
	"self": {}, "Self": {}, "super": {}, "static": {}, "operator": {},
	"type": {},
}

// snakeToPascal converts SCREAMING_SNAKE_CASE to PascalCase ("BAD_REQUEST" → "BadRequest").
func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		lower := strings.ToLower(p)
		if lower != "" {
			parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
		}
	}
	return strings.Join(parts, "")
}

// snakeToCamel converts SCREAMING_SNAKE_CASE to camelCase ("BAD_REQUEST" → "badRequest").
func snakeToCamel(s string) string {
	pascal := snakeToPascal(s)
	if pascal == "" {
		return ""
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// isSDKPkg reports whether pkgPath is api or api/v1 — the two packages
// whose struct types are emitted into the generated SDK.
func isSDKPkg(pkgPath string) bool {
	return pkgPath == reflect.TypeFor[v1.StatusResp]().PkgPath() ||
		pkgPath == reflect.TypeFor[api.ErrorResponse]().PkgPath()
}

// loadDocs parses Go source files in the current directory and extracts
// documentation comments, source file tracking, and alias type definitions.
func loadDocs() (*docRegistry, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	reg := &docRegistry{
		typeDoc:  map[string]string{},
		typeFile: map[string]string{},
		fieldDoc: map[string]map[string]string{},
	}

	// Pass 1: collect struct types and string alias declarations.
	stringAliases := map[string]string{} // type name → source file
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch t := typeSpec.Type.(type) {
				case *ast.StructType:
					var doc string
					if typeSpec.Doc != nil {
						doc = typeSpec.Doc.Text()
					} else if genDecl.Doc != nil && len(genDecl.Specs) == 1 {
						doc = genDecl.Doc.Text()
					}
					fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
					reg.typeFile[typeSpec.Name.Name] = fn
					if doc = strings.TrimSpace(doc); doc != "" {
						reg.typeDoc[typeSpec.Name.Name] = doc
					}
					fieldDocs := map[string]string{}
					for _, field := range t.Fields.List {
						var fdoc string
						if field.Doc != nil {
							fdoc = strings.TrimSpace(field.Doc.Text())
						} else if field.Comment != nil {
							fdoc = strings.TrimSpace(field.Comment.Text())
						}
						if fdoc != "" {
							for _, name := range field.Names {
								fieldDocs[name.Name] = fdoc
							}
						}
					}
					if len(fieldDocs) > 0 {
						reg.fieldDoc[typeSpec.Name.Name] = fieldDocs
					}
				case *ast.Ident:
					if t.Name == "string" {
						fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
						stringAliases[typeSpec.Name.Name] = fn
						reg.typeFile[typeSpec.Name.Name] = fn
					}
				case *ast.ArrayType:
					// Named slice type.
					fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
					reg.typeFile[typeSpec.Name.Name] = fn
					var doc string
					if typeSpec.Doc != nil {
						doc = typeSpec.Doc.Text()
					} else if genDecl.Doc != nil && len(genDecl.Specs) == 1 {
						doc = genDecl.Doc.Text()
					}
					if doc = strings.TrimSpace(doc); doc != "" {
						reg.typeDoc[typeSpec.Name.Name] = doc
					}
				}
			}
		}
	}

	// Pass 2: collect constants for string alias types.
	aliasConsts := map[string][]aliasConstant{}
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.CONST {
				continue
			}
			var lastType string
			for _, spec := range genDecl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typeName := lastType
				if vs.Type != nil {
					if ident, ok := vs.Type.(*ast.Ident); ok {
						typeName = ident.Name
						lastType = typeName
					}
				}
				if _, isAlias := stringAliases[typeName]; !isAlias {
					continue
				}
				for _, name := range vs.Names {
					if len(vs.Values) == 0 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					aliasConsts[typeName] = append(aliasConsts[typeName], aliasConstant{
						name:  name.Name,
						value: val,
					})
				}
			}
		}
	}

	// Build aliases in sorted order for deterministic output.
	aliasNames := slices.Sorted(maps.Keys(stringAliases))
	for _, name := range aliasNames {
		file := stringAliases[name]
		reg.aliases = append(reg.aliases, aliasInfo{
			name:      name,
			file:      file,
			constants: aliasConsts[name],
		})
	}

	reg.aliasNames = map[string]struct{}{}
	for _, a := range reg.aliases {
		reg.aliasNames[a.name] = struct{}{}
	}

	return reg, nil
}

// formatBlockDoc formats a doc string as a /** ... */ block comment with the given indent.
// Returns an empty string when doc is empty.
func formatBlockDoc(doc, indent string) string {
	if doc == "" {
		return ""
	}
	lines := strings.Split(doc, "\n")
	// Drop trailing empty lines.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) == 1 {
		return indent + "/** " + lines[0] + " */\n"
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, l := range lines {
		if l == "" {
			b.WriteString(indent + " *\n")
		} else {
			b.WriteString(indent + " * " + l + "\n")
		}
	}
	b.WriteString(indent + " */\n")
	return b.String()
}

// walkSDKTypes traverses struct types reachable from seeds in post-order
// (leaves first), returning only api or api/v1 struct types.
func walkSDKTypes(seeds []reflect.Type) []reflect.Type {
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
		if t.Kind() != reflect.Struct || !isSDKPkg(t.PkgPath()) {
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
func collectNamedSlices(structs []sdkType) []reflect.Type {
	seen := map[reflect.Type]struct{}{}
	var order []reflect.Type
	for _, ks := range structs {
		for f := range ks.t.Fields() {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() != reflect.Slice || !isSDKPkg(ft.PkgPath()) {
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
func routeSeedTypes() []reflect.Type {
	var seeds []reflect.Type
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if r.Req != nil {
			seeds = append(seeds, r.Req)
		}
		seeds = append(seeds, r.Resp)
	}
	return seeds
}

// emitTSAlias writes a type alias with its const values.
func emitTSAlias(b *strings.Builder, a aliasInfo) {
	if len(a.constants) > 0 {
		// Emit union type for exhaustiveness checking.
		fmt.Fprintf(b, "export type %s =\n", a.name)
		for i, c := range a.constants {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(b, "  | %q", c.value)
		}
		b.WriteString(";\n")
	} else {
		fmt.Fprintf(b, "export type %s = string;\n", a.name)
	}
	if len(a.constants) > 0 {
		b.WriteString("/**\n")
		b.WriteString(" * Supported values.\n")
		b.WriteString(" */\n")
	}
	for _, c := range a.constants {
		fmt.Fprintf(b, "export const %s: %s = %q;\n", c.name, a.name, c.value)
	}
	b.WriteString("\n")
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
	if t.Kind() == reflect.Struct && isSDKPkg(t.PkgPath()) {
		return "validate" + t.Name(), nil
	}
	v, err := c.tsPrimitiveValidator(t, "v", pathLit)
	if err != nil {
		return "", err
	}
	return "(v) => " + v, nil
}

// docRegistry holds parsed documentation extracted from Go source files.
type docRegistry struct {
	cfg        *genConfig
	typeDoc    map[string]string            // Go type name → doc comment text
	typeFile   map[string]string            // Go type name → source filename (e.g. "events.go")
	fieldDoc   map[string]map[string]string // Go type name → Go field name → doc comment text
	aliases    []aliasInfo                  // named-string types with their constants
	aliasNames map[string]struct{}          // set of alias type names for all target languages
}

// discoverSSEStructs returns the structs reachable from the SSE event types
// (EventMessage, TaskListEvent, UsageResp) in dependency order.
func (d *docRegistry) discoverSSEStructs() []sdkType {
	order := walkSDKTypes(d.cfg.sseSeeds)
	result := make([]sdkType, len(order))
	for i, t := range order {
		result[i] = sdkType{t: t}
	}
	return result
}

func writeTSJSONMethod(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	respType := r.RespName()
	if r.IsArray {
		respType += "[]"
	}

	args := make([]string, 0, len(params)+len(r.QueryParams)+1)
	for _, p := range params {
		args = append(args, p+": string")
	}
	for _, q := range r.QueryParams {
		args = append(args, q+": string")
	}
	hasReq := r.Req != nil
	if hasReq {
		args = append(args, "req: "+r.ReqName())
	}

	tsPath := buildTSPath(r.Path, params, r.QueryParams)

	if hasReq {
		fmt.Fprintf(b, "    %s: (%s): Promise<%s> => request<%s>(%q, %s, req),\n", r.Name, strings.Join(args, ", "), respType, respType, r.Method, tsPath)
	} else {
		fmt.Fprintf(b, "    %s: (%s): Promise<%s> => request<%s>(%q, %s),\n", r.Name, strings.Join(args, ", "), respType, respType, r.Method, tsPath)
	}
}

func writeTSSSEMethod(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params)+1)
	for _, p := range params {
		args = append(args, p+": string")
	}
	tsPath := buildTSPath(r.Path, params, nil)
	respName := r.RespName()
	validatorName := "validate" + respName
	args = append(args, "onMessage: (event: "+respName+") => void", "onError: (err: unknown) => void")
	fmt.Fprintf(b, "    %s: (%s): EventSource => {\n", r.Name, strings.Join(args, ", "))
	fmt.Fprintf(b, "      const es = new EventSource(%s);\n", tsPath)
	b.WriteString("      es.addEventListener(\"message\", (e) => {\n")
	b.WriteString("        try {\n")
	fmt.Fprintf(b, "          onMessage(%s(JSON.parse(e.data)));\n", validatorName)
	b.WriteString("        } catch (err) {\n")
	b.WriteString("          onError(err);\n")
	b.WriteString("        }\n")
	b.WriteString("      });\n")
	b.WriteString("      return es;\n")
	b.WriteString("    },\n")
}

// discoverKotlinStructs walks the API struct types reachable from route
// types and returns them in dependency order (leaves first).
func (d *docRegistry) discoverKotlinStructs() []sdkType {
	seeds := append(routeSeedTypes(), reflect.TypeFor[api.ErrorResponse]())
	order := walkSDKTypes(seeds)
	result := make([]sdkType, len(order))
	for i, t := range order {
		result[i] = sdkType{t: t, comment: d.cfg.sectionComments[t.Name()]}
	}
	return result
}

// needsSerialName returns true when the JSON name contains a run of two or
// more consecutive uppercase ASCII letters (e.g. "repoURL", "sessionID",
// "costUSD", "durationAPIMs"). Kotlin properties use these names as-is, but
// kotlinx.serialization would mangle them without an explicit @SerialName.
func needsSerialName(name string) bool {
	consecutive := 0
	for _, r := range name {
		if unicode.IsUpper(r) {
			consecutive++
			if consecutive >= 2 {
				return true
			}
		} else {
			consecutive = 0
		}
	}
	return false
}

// writeFieldDecl writes a single "val name: Type" (with optional "? = null").
func writeFieldDecl(b *strings.Builder, f *kotlinField) {
	if f.nullable {
		fmt.Fprintf(b, "val %s: %s? = null", f.ktName, f.ktType)
	} else {
		fmt.Fprintf(b, "val %s: %s", f.ktName, f.ktType)
	}
}

// fieldsNeedAnnotation returns true if any field requires @SerialName.
func fieldsNeedAnnotation(fields []kotlinField) bool {
	for _, f := range fields {
		if f.serialName != "" {
			return true
		}
	}
	return false
}

// parseJSONTag splits a json struct tag value into name and options.
func parseJSONTag(tag string) (name string, opts []string) {
	name, rest, ok := strings.Cut(tag, ",")
	if !ok || rest == "" {
		return name, nil
	}
	return name, strings.Split(rest, ",")
}

func writeKotlinClient(outDir string) error {
	var b strings.Builder

	// Static header and imports.
	b.WriteString(`// Code generated by gen-api-sdk. DO NOT EDIT.
package com.caic.sdk.v1

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.sse.EventSource
import okhttp3.sse.EventSourceListener
import okhttp3.sse.EventSources
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

class ApiException(
    val statusCode: Int,
    val code: String,
    message: String,
    val details: Map<String, kotlinx.serialization.json.JsonElement>? = null,
) : Exception(message)

class ApiClient(
    baseURL: String,
    private val tokenProvider: (() -> String?)? = null,
) {
    private val baseURL: String = baseURL.trimEnd('/')
    private val client = OkHttpClient()
    private val json = Json { ignoreUnknownKeys = true }
    private val jsonMediaType = "application/json".toMediaType()

    private suspend inline fun <reified T> request(method: String, path: String, body: String? = null): T {
        val url = "$baseURL$path"
        val needsBody = method in listOf("POST", "PUT", "PATCH")
        val requestBody = body?.toRequestBody(jsonMediaType)
            ?: if (needsBody) "".toRequestBody(jsonMediaType) else null
        val request = Request.Builder()
            .url(url)
            .method(method, requestBody)
            .header("Content-Type", "application/json")
            .apply { tokenProvider?.invoke()?.let { header("Authorization", "Bearer $it") } }
            .build()
        return suspendCancellableCoroutine { cont ->
            val call = client.newCall(request)
            cont.invokeOnCancellation { call.cancel() }
            call.enqueue(object : okhttp3.Callback {
                override fun onFailure(call: okhttp3.Call, e: java.io.IOException) {
                    cont.resumeWithException(e)
                }
                override fun onResponse(call: okhttp3.Call, response: Response) {
                    response.use { resp ->
                        val responseBody = resp.body?.string() ?: ""
                        if (!resp.isSuccessful) {
                            try {
                                val err = json.decodeFromString<ErrorResponse>(responseBody)
                                cont.resumeWithException(
                                    ApiException(resp.code, err.error.code, err.error.message, err.details)
                                )
                            } catch (_: Exception) {
                                cont.resumeWithException(
                                    ApiException(resp.code, "UNKNOWN", responseBody)
                                )
                            }
                            return
                        }
                        try {
                            cont.resume(json.decodeFromString<T>(responseBody))
                        } catch (e: Exception) {
                            cont.resumeWithException(e)
                        }
                    }
                }
            })
        }
    }

`)

	// Generate JSON endpoint methods from routes.
	b.WriteString("    // JSON endpoints\n")
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeKotlinJSONFunc(&b, r, params)
	}
	b.WriteString("\n")

	// Generate SSE endpoint methods from routes.
	b.WriteString("    // SSE endpoints\n")
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if !r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeKotlinSSEFunc(&b, r, params)
	}
	b.WriteString("\n")

	// sseFlow helper and reconnecting wrappers.
	b.WriteString(`    private inline fun <reified T> sseFlow(path: String): Flow<T> = callbackFlow {
        val request = Request.Builder()
            .url("$baseURL$path")
            .header("Accept", "text/event-stream")
            .apply { tokenProvider?.invoke()?.let { header("Authorization", "Bearer $it") } }
            .build()
        val factory = EventSources.createFactory(client)
        val source = factory.newEventSource(request, object : EventSourceListener() {
            override fun onEvent(eventSource: EventSource, id: String?, type: String?, data: String) {
                try {
                    val event = json.decodeFromString<T>(data)
                    trySend(event)
                } catch (_: Exception) {
                    // Skip malformed events.
                }
            }
            override fun onFailure(eventSource: EventSource, t: Throwable?, response: Response?) {
                close(t?.let { java.io.IOException("SSE connection failed", it) })
            }
            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    // Reconnecting SSE wrappers with exponential backoff.
`)

	// Generate reconnecting wrappers for SSE routes.
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if !r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeKotlinReconnectingFunc(&b, r, params)
	}
	b.WriteString("\n")

	b.WriteString(`    private fun <T> reconnectingFlow(connect: () -> Flow<T>): Flow<T> = flow {
        var delayMs = 500L
        while (true) {
            try {
                connect().onEach { delayMs = 500L }.collect { emit(it) }
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
                delay(jitteredDelay(delayMs))
                delayMs = (delayMs * 3 / 2).coerceAtMost(30_000L)
            }
        }
    }

    private fun jitteredDelay(base: Long): Long = (base * (0.75 + Math.random() * 0.5)).toLong()
}
`)

	return os.WriteFile(filepath.Join(outDir, "ApiClient.kt"), []byte(b.String()), 0o600)
}

func writeKotlinJSONFunc(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	respType := r.RespName()
	if r.IsArray {
		respType = "List<" + respType + ">"
	}

	// Build function parameters.
	args := make([]string, 0, len(params)+len(r.QueryParams)+1)
	for _, p := range params {
		args = append(args, p+": String")
	}
	for _, q := range r.QueryParams {
		args = append(args, q+": String")
	}
	hasReq := r.Req != nil
	if hasReq {
		args = append(args, "req: "+r.ReqName())
	}

	ktPath := buildKotlinPath(r.Path, r.QueryParams)

	sig := strings.Join(args, ", ")
	if hasReq {
		fmt.Fprintf(b, "    suspend fun %s(%s): %s = request(%q, %s, json.encodeToString(req))\n", r.Name, sig, respType, r.Method, ktPath)
	} else {
		fmt.Fprintf(b, "    suspend fun %s(%s): %s = request(%q, %s)\n", r.Name, sig, respType, r.Method, ktPath)
	}
}

func writeKotlinSSEFunc(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params))
	for _, p := range params {
		args = append(args, p+": String")
	}
	ktPath := buildKotlinPath(r.Path, nil)
	respName := r.RespName()
	fmt.Fprintf(b, "    fun %s(%s): Flow<%s> = sseFlow<%s>(%s)\n", r.Name, strings.Join(args, ", "), respName, respName, ktPath)
}

func writeKotlinReconnectingFunc(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	// Build the function name: e.g. "taskEvents" -> "taskEventsReconnecting"
	reconnectName := r.Name + "Reconnecting"

	allParams := slices.Concat(params, r.QueryParams)
	args := make([]string, 0, len(allParams))
	callArgs := make([]string, 0, len(allParams))
	for _, p := range allParams {
		args = append(args, p+": String")
		callArgs = append(callArgs, p)
	}

	fmt.Fprintf(b, "    fun %s(%s): Flow<%s> = reconnectingFlow { %s(%s) }\n",
		reconnectName, strings.Join(args, ", "), r.RespName(), r.Name, strings.Join(callArgs, ", "))
}

// buildKotlinPath returns a Kotlin string expression for the path. Uses string
// templates for paths with parameters and appends query params if any.
func buildKotlinPath(path string, queryParams []string) string {
	var b strings.Builder
	b.WriteString(pathParamRe.ReplaceAllStringFunc(path, func(match string) string {
		name := match[1 : len(match)-1]
		return "$" + name
	}))
	for i, q := range queryParams {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(q)
		b.WriteString("=$")
		b.WriteString(q)
	}
	return fmt.Sprintf("%q", b.String())
}

func extractPathParams(path string) []string {
	matches := pathParamRe.FindAllStringSubmatch(path, -1)
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m[1]
	}
	return out
}

// buildTSPath returns either a quoted string or a template literal for paths
// with path params and/or query params.
func buildTSPath(path string, params, queryParams []string) string {
	if len(params) == 0 && len(queryParams) == 0 {
		return fmt.Sprintf("%q", path)
	}
	var b strings.Builder
	b.WriteString(pathParamRe.ReplaceAllStringFunc(path, func(match string) string {
		name := match[1 : len(match)-1]
		return "${" + name + "}"
	}))
	for i, q := range queryParams {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(q)
		b.WriteString("=${encodeURIComponent(")
		b.WriteString(q)
		b.WriteByte(')')
		b.WriteByte('}')
	}
	return "`" + b.String() + "`"
}

// docGroupRoutes groups routes by CategoryName(), preserving first-seen order.
func docGroupRoutes(routes []v1.Route) []docRouteGroup {
	seen := map[string]int{} // category name → index in result
	var result []docRouteGroup
	for i := range routes {
		r := &routes[i]
		cat := r.CategoryName()
		if idx, ok := seen[cat]; ok {
			result[idx].routes = append(result[idx].routes, *r)
		} else {
			seen[cat] = len(result)
			result = append(result, docRouteGroup{name: cat, routes: []v1.Route{*r}})
		}
	}
	return result
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
		if isSDKPkg(t.PkgPath()) {
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

// swiftEscapeIdent wraps name in backticks if it is a Swift reserved word.
func swiftEscapeIdent(name string) string {
	if _, ok := swiftReservedWords[name]; ok {
		return "`" + name + "`"
	}
	return name
}

// formatSwiftDoc formats a doc string as Swift triple-slash documentation comments.
// Returns an empty string when doc is empty.
func formatSwiftDoc(doc, indent string) string {
	if doc == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(doc), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines {
		if l == "" {
			b.WriteString(indent + "///\n")
		} else {
			b.WriteString(indent + "/// " + l + "\n")
		}
	}
	return b.String()
}

// buildSwiftPath returns a Swift string literal for the path.
// Path params are replaced with Swift string interpolation \(name).
// Query params are appended as URL-encoded interpolated strings.
func buildSwiftPath(path string, queryParams []string) string {
	var b strings.Builder
	b.WriteString(pathParamRe.ReplaceAllStringFunc(path, func(match string) string {
		name := match[1 : len(match)-1]
		return "\\(" + name + ")"
	}))
	for i, q := range queryParams {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(q + "=\\(" + q + ".addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? " + q + ")")
	}
	return "\"" + b.String() + "\""
}

// discoverSwiftStructs walks the API struct types reachable from route types
// and returns them in dependency order, annotated with Swift section comments.
func (d *docRegistry) discoverSwiftStructs() []sdkType {
	seeds := append(routeSeedTypes(), reflect.TypeFor[api.ErrorResponse]())
	order := walkSDKTypes(seeds)
	result := make([]sdkType, len(order))
	for i, t := range order {
		result[i] = sdkType{t: t, comment: d.cfg.sectionComments[t.Name()]}
	}
	return result
}

func writeSwiftJSONFunc(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatSwiftDoc(r.Doc, "    "))
	}
	respType := r.RespName()
	if r.IsArray {
		respType = "[" + respType + "]"
	}

	args := make([]string, 0, len(params)+len(r.QueryParams)+1)
	for _, p := range params {
		args = append(args, p+": String")
	}
	for _, q := range r.QueryParams {
		args = append(args, q+": String")
	}
	hasReq := r.Req != nil
	if hasReq {
		args = append(args, "req: "+r.ReqName())
	}
	swiftPath := buildSwiftPath(r.Path, r.QueryParams)

	fmt.Fprintf(b, "    public func %s(%s) async throws -> %s {\n", r.Name, strings.Join(args, ", "), respType)
	if hasReq {
		fmt.Fprintf(b, "        try await request(%q, path: %s, body: try encoder.encode(req))\n", r.Method, swiftPath)
	} else {
		fmt.Fprintf(b, "        try await request(%q, path: %s)\n", r.Method, swiftPath)
	}
	b.WriteString("    }\n")
}

func writeSwiftSSEFunc(b *strings.Builder, r *v1.Route, params []string) {
	if r.Doc != "" {
		b.WriteString(formatSwiftDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params))
	for _, p := range params {
		args = append(args, p+": String")
	}
	swiftPath := buildSwiftPath(r.Path, nil)
	respName := r.RespName()
	fmt.Fprintf(b, "    public func %s(%s) -> AsyncThrowingStream<%s, Error> {\n", r.Name, strings.Join(args, ", "), respName)
	fmt.Fprintf(b, "        sseStream(path: %s)\n", swiftPath)
	b.WriteString("    }\n")
}

func writeSwiftReconnectingFunc(b *strings.Builder, r *v1.Route, params []string) {
	allParams := slices.Concat(params, r.QueryParams)
	args := make([]string, 0, len(allParams))
	callArgs := make([]string, 0, len(allParams))
	for _, p := range allParams {
		args = append(args, p+": String")
		callArgs = append(callArgs, p+": "+p)
	}
	reconnectName := r.Name + "Reconnecting"
	respName := r.RespName()
	fmt.Fprintf(b, "    public func %s(%s) -> AsyncThrowingStream<%s, Error> {\n",
		reconnectName, strings.Join(args, ", "), respName)
	fmt.Fprintf(b, "        reconnectingStream { self.%s(%s) }\n", r.Name, strings.Join(callArgs, ", "))
	b.WriteString("    }\n")
}

func writeSwiftClient(outDir string) error {
	var b strings.Builder

	b.WriteString(`// Code generated by gen-api-sdk. DO NOT EDIT.
import Foundation

public struct ApiError: Error {
    public let statusCode: Int
    public let code: String
    public let message: String
    public let details: [String: JSONValue]?
}

public final class ApiClient {
    private let baseURL: String
    private let tokenProvider: (() -> String?)?
    private let urlSession: URLSession
    private let encoder = JSONEncoder()
    private let decoder = JSONDecoder()

    public init(baseURL: String, tokenProvider: (() -> String?)? = nil) {
        self.baseURL = baseURL.hasSuffix("/") ? String(baseURL.dropLast()) : baseURL
        self.tokenProvider = tokenProvider
        self.urlSession = URLSession(configuration: .default)
    }

    private func request<T: Decodable>(_ method: String, path: String, body: Data? = nil) async throws -> T {
        var req = URLRequest(url: URL(string: baseURL + path)!)
        req.httpMethod = method
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.timeoutInterval = 60
        if let token = tokenProvider?() {
            req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        if let body {
            req.httpBody = body
        } else if ["POST", "PUT", "PATCH"].contains(method) {
            req.httpBody = Data()
        }
        let (data, response) = try await urlSession.data(for: req)
        let httpResponse = response as! HTTPURLResponse
        guard (200..<300).contains(httpResponse.statusCode) else {
            if let errResp = try? decoder.decode(ErrorResponse.self, from: data) {
                throw ApiError(statusCode: httpResponse.statusCode, code: errResp.error.code,
                               message: errResp.error.message, details: nil)
            }
            throw ApiError(statusCode: httpResponse.statusCode, code: "UNKNOWN",
                           message: String(data: data, encoding: .utf8) ?? "", details: nil)
        }
        return try decoder.decode(T.self, from: data)
    }

    private func sseStream<T: Decodable>(path: String) -> AsyncThrowingStream<T, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    var req = URLRequest(url: URL(string: self.baseURL + path)!)
                    req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    if let token = self.tokenProvider?() {
                        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
                    }
                    let (bytes, response) = try await self.urlSession.bytes(for: req)
                    if let httpResponse = response as? HTTPURLResponse,
                       !(200..<300).contains(httpResponse.statusCode) {
                        continuation.finish(throwing: ApiError(
                            statusCode: httpResponse.statusCode, code: "HTTP_ERROR",
                            message: "SSE connection failed with status \(httpResponse.statusCode)",
                            details: nil))
                        return
                    }
                    for try await line in bytes.lines {
                        if Task.isCancelled { break }
                        guard line.hasPrefix("data: ") else { continue }
                        let data = Data(line.dropFirst(6).utf8)
                        if let event = try? self.decoder.decode(T.self, from: data) {
                            continuation.yield(event)
                        }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    private func reconnectingStream<T>(_ connect: @escaping () -> AsyncThrowingStream<T, Error>) -> AsyncThrowingStream<T, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                var delayNs: UInt64 = 500_000_000
                while !Task.isCancelled {
                    do {
                        for try await value in connect() {
                            delayNs = 500_000_000
                            continuation.yield(value)
                        }
                        continuation.finish()
                        return
                    } catch {
                        if Task.isCancelled { break }
                        try? await Task.sleep(nanoseconds: delayNs)
                        delayNs = min(delayNs * 3 / 2, 4_000_000_000)
                    }
                }
                continuation.finish()
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

`)

	// JSON endpoint methods.
	b.WriteString("    // JSON endpoints\n")
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeSwiftJSONFunc(&b, r, params)
	}
	b.WriteString("\n")

	// SSE endpoint methods.
	b.WriteString("    // SSE endpoints\n")
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if !r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeSwiftSSEFunc(&b, r, params)
	}
	b.WriteString("\n")

	// Reconnecting SSE wrappers.
	b.WriteString("    // Reconnecting SSE wrappers with exponential backoff\n")
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if !r.IsSSE {
			continue
		}
		params := extractPathParams(r.Path)
		writeSwiftReconnectingFunc(&b, r, params)
	}
	b.WriteString("}\n")

	return os.WriteFile(filepath.Join(outDir, "ApiClient.swift"), []byte(b.String()), 0o600)
}

// discoverTSStructs walks route types and ErrorResponse, and annotates
// each struct with its source file for section grouping.
func (d *docRegistry) discoverTSStructs() []sdkType {
	seeds := append(routeSeedTypes(), reflect.TypeFor[api.ErrorResponse]())
	order := walkSDKTypes(seeds)
	result := make([]sdkType, len(order))
	for i, t := range order {
		result[i] = sdkType{t: t, comment: d.typeFile[t.Name()]}
	}
	return result
}

// goTypeToTS maps a Go reflect.Type to its TypeScript type string.
func (d *docRegistry) goTypeToTS(t reflect.Type) (string, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if m := d.cfg.lookupSpecial(t); m != nil {
		return m.tsType, nil
	}
	switch t {
	case reflect.TypeFor[map[string]bool]():
		return "{ [key: string]: boolean}", nil
	case reflect.TypeFor[map[string]string]():
		return "{ [key: string]: string}", nil
	}

	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name(), nil
	}

	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Int:
		return "number /* int */", nil
	case reflect.Int64:
		return "number /* int64 */", nil
	case reflect.Uint64:
		return "number /* uint64 */", nil
	case reflect.Float64:
		return "number /* float64 */", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Slice:
		if isSDKPkg(t.PkgPath()) {
			return t.Name(), nil // Named slice.
		}
		e, err := d.goTypeToTS(t.Elem())
		if err != nil {
			return "", err
		}
		return e + "[]", nil
	case reflect.Map:
		k, err := d.goTypeToTS(t.Key())
		if err != nil {
			return "", err
		}
		v, err := d.goTypeToTS(t.Elem())
		if err != nil {
			return "", err
		}
		return "{ [key: " + k + "]: " + v + "}", nil
	case reflect.Struct:
		return t.Name(), nil
	default:
		return "", fmt.Errorf("goTypeToTS: unhandled kind %s for %s", t.Kind(), t)
	}
}

// emitTSStruct writes a TypeScript interface to b.
func (d *docRegistry) emitTSStruct(b *strings.Builder, t reflect.Type) error {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatBlockDoc(doc, ""))
	}
	name := t.Name()
	fmt.Fprintf(b, "export interface %s {\n", name)
	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		omit := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero")
		isPtr := sf.Type.Kind() == reflect.Pointer
		optional := isPtr || (omit && !isPtr)
		tsType, err := d.goTypeToTS(sf.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, jsonName, err)
		}

		// Field-level doc: block comment for multi-line, inline for single-line.
		fdoc := ""
		if fdocs, ok := d.fieldDoc[name]; ok {
			fdoc = fdocs[sf.Name]
		}
		if fdoc != "" {
			if strings.Contains(fdoc, "\n") {
				b.WriteString(formatBlockDoc(fdoc, "  "))
			} else {
				fmt.Fprintf(b, "  /** %s */\n", fdoc)
			}
		}

		if optional {
			fmt.Fprintf(b, "  %s?: %s;\n", jsonName, tsType)
		} else {
			fmt.Fprintf(b, "  %s: %s;\n", jsonName, tsType)
		}
	}
	b.WriteString("}\n")
	return nil
}

// generateTSTypes generates sdk/caic/ts/v1/types.gen.ts from the Go DTO structs.
func (d *docRegistry) generateTSTypes(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n")
	b.WriteString("/** ISO 8601 timestamp string (e.g. \"2026-04-13T12:00:00Z\"). */\n")
	b.WriteString("export type ISOTimestamp = string & { readonly __brand: \"ISOTimestamp\" };\n\n")

	allStructs := d.discoverTSStructs()

	// Group structs by source file for section headers.
	structsBySource := map[string][]sdkType{}
	for _, ks := range allStructs {
		src := ks.comment
		if src == "" {
			src = "types.go"
		}
		structsBySource[src] = append(structsBySource[src], ks)
	}

	// We use the order from allStructs (walk order, leaves-first within each
	// source file group) and filter for the current source file.
	seen := map[string]struct{}{}

	// Emit events.go section with its type aliases first.
	if _, ok := structsBySource["events.go"]; ok {
		b.WriteString("//////////\n")
		b.WriteString("// source: events.go\n")
		b.WriteString("/*\n")
		b.WriteString("SSE event types sent to the frontend for task event streams.\n")
		b.WriteString("*/\n\n")

		for i := range d.aliases {
			a := &d.aliases[i]
			if a.file != "events.go" {
				continue
			}
			if _, ok := seen[a.name]; ok {
				continue
			}
			emitTSAlias(&b, *a)
			seen[a.name] = struct{}{}
		}

		// Then structs in walk order, filtered to events.go.
		for _, ks := range allStructs {
			name := ks.t.Name()
			if _, ok := seen[name]; ok || ks.comment != "events.go" {
				continue
			}
			seen[name] = struct{}{}
			if err := d.emitTSStruct(&b, ks.t); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			b.WriteString("\n")
		}
		delete(structsBySource, "events.go")
	}

	// Emit types.go section.
	b.WriteString("\n//////////\n")
	b.WriteString("// source: types.go\n\n")

	// Type aliases from types.go (all non-events aliases).
	for _, a := range d.aliases {
		if _, ok := seen[a.name]; ok {
			continue
		}
		emitTSAlias(&b, a)
		seen[a.name] = struct{}{}
	}

	// Named slice types used as fields in discovered structs.
	namedSlices := collectNamedSlices(allStructs)
	for _, ns := range namedSlices {
		name := ns.Name()
		if _, ok := seen[name]; ok {
			continue
		}
		if doc := d.typeDoc[name]; doc != "" {
			b.WriteString(formatBlockDoc(doc, ""))
		}
		elemType, err := d.cfg.goTypeToDoc(ns.Elem())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(&b, "export type %s = %s[];\n\n", name, elemType)
		seen[name] = struct{}{}
	}

	// Structs from types.go (and any ungrouped) in walk order.
	for _, ks := range allStructs {
		name := ks.t.Name()
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if err := d.emitTSStruct(&b, ks.t); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "types.gen.ts"), []byte(b.String()), 0o600)
}

// tsFieldValidator returns a TypeScript expression that validates a value
// at the given pathExpr. pathLit is a string literal for error messages.
func (d *docRegistry) tsFieldValidator(t reflect.Type, pathExpr, pathLit string) (string, error) {
	if t.Kind() == reflect.Pointer {
		return d.tsFieldValidator(t.Elem(), pathExpr, pathLit)
	}
	if m := d.cfg.lookupSpecial(t); m != nil && m.tsValidate != "" {
		return fmt.Sprintf(m.tsValidate, pathExpr, pathLit), nil
	}
	if t.Kind() == reflect.Slice {
		elemFn, err := d.cfg.tsElemValidatorFunc(t.Elem(), pathLit+"[i]")
		if err != nil {
			return "", err
		}
		elemTS, err := d.goTypeToTS(t.Elem())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("validateArray(%s, %q, %s) as %s[]", pathExpr, pathLit, elemFn, elemTS), nil
	}

	if t.Kind() == reflect.Map {
		valFn, err := d.cfg.tsElemValidatorFunc(t.Elem(), pathLit+"[k]")
		if err != nil {
			return "", err
		}
		keyTS, err := d.goTypeToTS(t.Key())
		if err != nil {
			return "", err
		}
		valTS, err := d.goTypeToTS(t.Elem())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("validateRecord(%s, %q, %s) as { [key: %s]: %s }", pathExpr, pathLit, valFn, keyTS, valTS), nil
	}
	if t.Kind() == reflect.Struct && isSDKPkg(t.PkgPath()) {
		return fmt.Sprintf("validate%s(%s)", t.Name(), pathExpr), nil
	}
	expr, err := d.cfg.tsPrimitiveValidator(t, pathExpr, pathLit)
	if err != nil {
		return "", err
	}
	if _, ok := d.aliasNames[t.Name()]; ok {
		return fmt.Sprintf("(%s as %s)", expr, t.Name()), nil
	}
	return expr, nil
}

// tsFieldOptValidator is like tsFieldValidator but wraps the result to
// return undefined when the value is null/undefined.
func (d *docRegistry) tsFieldOptValidator(t reflect.Type, pathExpr, pathLit string) (string, error) {
	inner, err := d.tsFieldValidator(t, pathExpr, pathLit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s === undefined || %s === null ? undefined : %s)", pathExpr, pathExpr, inner), nil
}

// emitTSDiscriminator writes a discriminated union validator with a kind-based
// dispatch. Fields without omitempty are base fields extracted before the switch
// (except "kind", which is always the discriminator). Fields with omitempty or
// pointer types are variant fields dispatched by their json name.
func (d *docRegistry) emitTSDiscriminator(b *strings.Builder, t reflect.Type) error {
	name := t.Name()
	fmt.Fprintf(b, "export function validate%s(raw: unknown): %s {\n", name, name)
	fmt.Fprintf(b, "  const obj = asObject(raw, %q);\n", name)

	// Collect base fields (non-pointer, no omitempty, not "kind").
	var kindExpr string
	for sf := range t.Fields() {
		if sf.IsExported() {
			tag := sf.Tag.Get("json")
			jsonName, _ := parseJSONTag(tag)
			if jsonName == "kind" || (jsonName == "" && sf.Name == "Kind") {
				expr := "asString(obj[\"kind\"], \"" + name + ".kind\")"
				if _, ok := d.aliasNames[sf.Type.Name()]; ok {
					expr = "(" + expr + " as " + sf.Type.Name() + ")"
				}
				kindExpr = expr
				break
			}
		}
	}
	if kindExpr == "" {
		kindExpr = "asString(obj[\"kind\"], \"" + name + ".kind\")"
	}
	baseFields := []tsFieldBinding{{"kind", kindExpr}}
	var variantFields []tsFieldBinding

	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		if jsonName == "kind" {
			continue
		}
		omit := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero")
		isPtr := sf.Type.Kind() == reflect.Pointer
		pathExpr := fmt.Sprintf("obj[%q]", jsonName)
		pathLit := fmt.Sprintf("%s.%s", name, jsonName)

		expr, err := d.tsFieldValidator(sf.Type, pathExpr, pathLit)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, jsonName, err)
		}
		if !omit && !isPtr {
			// Base field: extracted before the switch.
			baseFields = append(baseFields, tsFieldBinding{jsonName, expr})
		} else {
			// Variant field: extracted in the switch.
			variantFields = append(variantFields, tsFieldBinding{jsonName, expr})
		}
	}

	b.WriteString("  const result: " + name + " = {\n")
	for _, bf := range baseFields {
		fmt.Fprintf(b, "    %s: %s,\n", bf.jsonName, bf.expr)
	}
	b.WriteString("  };\n")
	b.WriteString("  switch (result.kind) {\n")
	for _, vf := range variantFields {
		fmt.Fprintf(b, "    case %q:\n", vf.jsonName)
		fmt.Fprintf(b, "      result.%s = %s;\n", vf.jsonName, vf.expr)
		b.WriteString("      break;\n")
	}
	b.WriteString("    // Unknown kinds pass through.\n")
	b.WriteString("  }\n")
	b.WriteString("  return result;\n")
	b.WriteString("}\n\n")
	return nil
}

// emitTSValidator writes a validateXxx(raw: unknown): Xxx function for a struct.
func (d *docRegistry) emitTSValidator(b *strings.Builder, t reflect.Type) error {
	name := t.Name()
	fmt.Fprintf(b, "export function validate%s(raw: unknown): %s {\n", name, name)
	fmt.Fprintf(b, "  const obj = asObject(raw, %q);\n", name)
	fmt.Fprintf(b, "  return {\n")

	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		omit := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero")
		isPtr := sf.Type.Kind() == reflect.Pointer
		optional := isPtr || (omit && !isPtr)

		pathExpr := fmt.Sprintf("obj[%q]", jsonName)
		pathLit := fmt.Sprintf("%s.%s", name, jsonName)
		var expr string
		var err error
		if optional {
			expr, err = d.tsFieldOptValidator(sf.Type, pathExpr, pathLit)
		} else {
			expr, err = d.tsFieldValidator(sf.Type, pathExpr, pathLit)
		}
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, jsonName, err)
		}
		fmt.Fprintf(b, "    %s: %s,\n", jsonName, expr)
	}

	b.WriteString("  };\n")
	b.WriteString("}\n\n")
	return nil
}

// generateTSValidate generates sdk/caic/ts/v1/validate.gen.ts with runtime
// validators for SSE-relevant DTO structs.
func (d *docRegistry) generateTSValidate(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n\n")
	b.WriteString("// Runtime schema validators for SSE payloads.\n")
	b.WriteString("// Each validator checks structural correctness at runtime and throws\n")
	b.WriteString("// TypeError on mismatch. Unknown kinds pass through for forward compat.\n\n")

	// Collect all types referenced by validators. ISOTimestamp is the only
	// type not discoverable from Go struct reflection (it's a TypeScript alias
	// for time.Time).
	needed := d.discoverSSEStructs()
	refTypes := map[string]struct{}{}
	for _, ks := range needed {
		refTypes[ks.t.Name()] = struct{}{}
		// Collect alias types used by struct fields (for union type casts).
		for sf := range ks.t.Fields() {
			if !sf.IsExported() {
				continue
			}
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if _, ok := d.aliasNames[ft.Name()]; ok {
				refTypes[ft.Name()] = struct{}{}
			}
		}
	}
	refTypes["ISOTimestamp"] = struct{}{}

	if len(refTypes) > 0 {
		sorted := slices.Sorted(maps.Keys(refTypes))
		b.WriteString("import type { ")
		for i, name := range sorted {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(name)
		}
		b.WriteString(" } from \"./types.gen\";\n\n")
	}

	// Helper functions.
	b.WriteString("// ---- helpers ----\n\n")
	b.WriteString("function asObject(v: unknown, path: string): Record<string, unknown> {\n")
	b.WriteString("  if (typeof v !== \"object\" || v === null || Array.isArray(v)) {\n")
	b.WriteString("    throw new TypeError(path + \": expected object\");\n")
	b.WriteString("  }\n")
	b.WriteString("  return v as Record<string, unknown>;\n")
	b.WriteString("}\n\n")
	b.WriteString("function asString(v: unknown, path: string): string {\n")
	b.WriteString("  if (typeof v !== \"string\") {\n")
	b.WriteString("    throw new TypeError(path + \": expected string, got \" + typeof v);\n")
	b.WriteString("  }\n")
	b.WriteString("  return v;\n")
	b.WriteString("}\n\n")
	b.WriteString("function asNumber(v: unknown, path: string): number {\n")
	b.WriteString("  if (typeof v !== \"number\" || Number.isNaN(v)) {\n")
	b.WriteString("    throw new TypeError(path + \": expected number, got \" + typeof v);\n")
	b.WriteString("  }\n")
	b.WriteString("  return v;\n")
	b.WriteString("}\n\n")
	b.WriteString("function asBoolean(v: unknown, path: string): boolean {\n")
	b.WriteString("  if (typeof v !== \"boolean\") {\n")
	b.WriteString("    throw new TypeError(path + \": expected boolean, got \" + typeof v);\n")
	b.WriteString("  }\n")
	b.WriteString("  return v;\n")
	b.WriteString("}\n\n")
	b.WriteString("function validateArray(v: unknown, path: string, elemValidator: (v: unknown) => unknown): unknown[] {\n")
	b.WriteString("  if (!Array.isArray(v)) {\n")
	b.WriteString("    throw new TypeError(path + \": expected array, got \" + (v === null ? \"null\" : typeof v));\n")
	b.WriteString("  }\n")
	b.WriteString("  return v.map((e, i) => elemValidator(e));\n")
	b.WriteString("}\n\n")
	b.WriteString("function validateRecord(v: unknown, path: string, valValidator: (v: unknown) => unknown): Record<string, unknown> {\n")
	b.WriteString("  if (typeof v !== \"object\" || v === null || Array.isArray(v)) {\n")
	b.WriteString("    throw new TypeError(path + \": expected object, got \" + (v === null ? \"null\" : Array.isArray(v) ? \"array\" : typeof v));\n")
	b.WriteString("  }\n")
	b.WriteString("  const result: Record<string, unknown> = {};\n")
	b.WriteString("  for (const k of Object.keys(v as Record<string, unknown>)) {\n")
	b.WriteString("    result[k] = valValidator((v as Record<string, unknown>)[k]);\n")
	b.WriteString("  }\n")
	b.WriteString("  return result;\n")
	b.WriteString("}\n\n")

	// Generate validators for SSE-relevant structs: EventMessage sub-types,
	// TaskListEvent's referenced types, and UsageResp's referenced types.
	discriminated := map[string]struct{}{}
	for _, n := range d.cfg.discriminated {
		discriminated[n] = struct{}{}
	}
	emitted := map[string]struct{}{}

	for _, ks := range needed {
		name := ks.t.Name()
		if _, ok := emitted[name]; ok {
			continue
		}
		if _, ok := discriminated[name]; ok {
			if err := d.emitTSDiscriminator(&b, ks.t); err != nil {
				return fmt.Errorf("validate%s: %w", name, err)
			}
			emitted[name] = struct{}{}
			continue
		}
		emitted[name] = struct{}{}
		if err := d.emitTSValidator(&b, ks.t); err != nil {
			return fmt.Errorf("validate%s: %w", name, err)
		}
	}

	return os.WriteFile(filepath.Join(outDir, "validate.gen.ts"), []byte(b.String()), 0o600)
}

// generateTS generates the TypeScript API client as a createApiClient factory.
func (*docRegistry) generateTS(outDir string) error {
	// Collect all referenced types for the import statement.
	types := map[string]struct{}{}
	validators := map[string]struct{}{}
	for i := range v1.Routes {
		r := &v1.Routes[i]
		if n := r.ReqName(); n != "" {
			types[n] = struct{}{}
		}
		types[r.RespName()] = struct{}{}
		if r.IsSSE {
			validators["validate"+r.RespName()] = struct{}{}
		}
	}
	types["ErrorResponse"] = struct{}{}

	sorted := slices.Sorted(maps.Keys(types))

	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "import type { %s } from \"./types.gen\";\n", strings.Join(sorted, ", "))
	if len(validators) > 0 {
		sortedVal := slices.Sorted(maps.Keys(validators))
		fmt.Fprintf(&b, "import { %s } from \"./validate.gen\";\n", strings.Join(sortedVal, ", "))
	}
	b.WriteString("\n")

	// APIError class.
	b.WriteString(`export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
    public details?: Record<string, unknown>,
  ) {
    super(code);
  }
}

`)

	// FetchFn type and makeRequester factory.
	b.WriteString(`export type FetchFn = (url: string, init?: RequestInit) => Promise<Response>;

function makeRequester(fetchFn: FetchFn) {
  return async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const init: RequestInit = { method, headers: { "Content-Type": "application/json" }, signal: AbortSignal.timeout(60_000) };
    if (body !== undefined) init.body = JSON.stringify(body);
    const res = await fetchFn(path, init);
    if (!res.ok) {
      const err = (await res.json()) as ErrorResponse;
      const e = new APIError(res.status, err.error.code, err.details);
      e.message = err.error.message;
      throw e;
    }
    return res.json() as Promise<T>;
  };
}

`)

	// createApiClient factory function wrapping all methods.
	b.WriteString("// createApiClient returns an API client bound to the given fetch function.\n")
	b.WriteString("// When fetchFn is omitted, globalThis.fetch is used.\n")
	b.WriteString("// eslint-disable-next-line @typescript-eslint/no-explicit-any\n")
	b.WriteString("export function createApiClient(fetchFn: FetchFn = (globalThis as any).fetch.bind(globalThis)) {\n")
	b.WriteString("  const request = makeRequester(fetchFn);\n")
	b.WriteString("  return {\n")

	// One method per route.
	for i := range v1.Routes {
		r := &v1.Routes[i]
		params := extractPathParams(r.Path)
		if r.IsSSE {
			writeTSSSEMethod(&b, r, params)
		} else {
			writeTSJSONMethod(&b, r, params)
		}
	}

	b.WriteString("  };\n")
	b.WriteString("}\n")

	return os.WriteFile(filepath.Join(outDir, "api.gen.ts"), []byte(b.String()), 0o600)
}

// generateKotlin generates Types.kt and ApiClient.kt in outDir.
func (d *docRegistry) generateKotlin(outDir string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	if err := d.writeKotlinTypes(outDir); err != nil {
		return err
	}
	return writeKotlinClient(outDir)
}

func (d *docRegistry) goTypeToKotlin(t reflect.Type) (string, error) {
	// Unwrap pointer — nullability is handled by the caller.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	// Special cases.
	if m := d.cfg.lookupSpecial(t); m != nil {
		return m.ktType, nil
	}

	// Named string aliases (Harness, EventKind).
	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name(), nil
	}

	switch t.Kind() {
	case reflect.String:
		return "String", nil
	case reflect.Int:
		return "Int", nil
	case reflect.Int64:
		return "Long", nil
	case reflect.Uint64:
		return "Long", nil // JSON numbers; unsigned semantics not enforced at wire level.
	case reflect.Float64:
		return "Double", nil
	case reflect.Bool:
		return "Boolean", nil
	case reflect.Slice:
		if isSDKPkg(t.PkgPath()) {
			return t.Name(), nil // Named slice.
		}
		e, err := d.goTypeToKotlin(t.Elem())
		if err != nil {
			return "", err
		}
		return "List<" + e + ">", nil
	case reflect.Map:
		k, err := d.goTypeToKotlin(t.Key())
		if err != nil {
			return "", err
		}
		v, err := d.goTypeToKotlin(t.Elem())
		if err != nil {
			return "", err
		}
		return "Map<" + k + ", " + v + ">", nil
	case reflect.Struct:
		return t.Name(), nil
	default:
		return "", fmt.Errorf("goTypeToKotlin: unhandled kind %s for %s", t.Kind(), t)
	}
}

// parseStructFields extracts kotlinField entries from a reflect.Type.
func (d *docRegistry) parseStructFields(t reflect.Type) ([]kotlinField, error) {
	fields := make([]kotlinField, 0, t.NumField())
	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		omit := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero")

		ft := sf.Type
		isPtr := ft.Kind() == reflect.Pointer

		ktType, err := d.goTypeToKotlin(ft)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), jsonName, err)
		}
		nullable := isPtr || (omit && !isPtr)

		sn := ""
		if needsSerialName(jsonName) {
			sn = jsonName
		}

		fields = append(fields, kotlinField{
			jsonName:   jsonName,
			ktName:     jsonName,
			ktType:     ktType,
			nullable:   nullable,
			serialName: sn,
		})
	}
	return fields, nil
}

// emitKotlinStruct writes a @Serializable data class to b.
func (d *docRegistry) emitKotlinStruct(b *strings.Builder, t reflect.Type) error {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatBlockDoc(doc, ""))
	}
	fields, err := d.parseStructFields(t)
	if err != nil {
		return err
	}
	name := t.Name()

	// Compact single-line form for structs with ≤2 fields and no @SerialName.
	if len(fields) <= 2 && !fieldsNeedAnnotation(fields) {
		b.WriteString("@Serializable\n")
		fmt.Fprintf(b, "data class %s(", name)
		for i := range fields {
			if i > 0 {
				b.WriteString(", ")
			}
			writeFieldDecl(b, &fields[i])
		}
		b.WriteString(")\n")
		return nil
	}

	b.WriteString("@Serializable\n")
	fmt.Fprintf(b, "data class %s(\n", name)
	for i := range fields {
		f := &fields[i]
		b.WriteString("    ")
		if f.serialName != "" {
			fmt.Fprintf(b, "@SerialName(%q) ", f.serialName)
		}
		writeFieldDecl(b, f)
		b.WriteString(",\n")
	}
	b.WriteString(")\n")
	return nil
}

func (d *docRegistry) writeKotlinTypes(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n")
	b.WriteString("@file:UseSerializers(InstantSerializer::class)\n\n")
	b.WriteString("package com.caic.sdk.v1\n\n")
	b.WriteString("import java.time.Instant\n")
	b.WriteString("import kotlinx.serialization.KSerializer\n")
	b.WriteString("import kotlinx.serialization.SerialName\n")
	b.WriteString("import kotlinx.serialization.Serializable\n")
	b.WriteString("import kotlinx.serialization.UseSerializers\n")
	b.WriteString("import kotlinx.serialization.descriptors.PrimitiveKind\n")
	b.WriteString("import kotlinx.serialization.descriptors.PrimitiveSerialDescriptor\n")
	b.WriteString("import kotlinx.serialization.encoding.Decoder\n")
	b.WriteString("import kotlinx.serialization.encoding.Encoder\n")
	b.WriteString("import kotlinx.serialization.json.JsonElement\n\n")
	// InstantSerializer: converts between ISO 8601 strings and java.time.Instant.
	b.WriteString("/** Serializes [Instant] as an ISO 8601 string. */\n")
	b.WriteString("object InstantSerializer : KSerializer<Instant> {\n")
	b.WriteString("    override val descriptor = PrimitiveSerialDescriptor(\"Instant\", PrimitiveKind.STRING)\n")
	b.WriteString("    override fun serialize(encoder: Encoder, value: Instant) = encoder.encodeString(value.toString())\n")
	b.WriteString("    override fun deserialize(decoder: Decoder): Instant = Instant.parse(decoder.decodeString())\n")
	b.WriteString("}\n\n")

	// Sealed interfaces with serializer for exhaustive when.
	for i := range d.aliases {
		a := &d.aliases[i]
		// Sealed interface.
		fmt.Fprintf(&b, "@Serializable(with = %sSerializer::class)\n", a.name)
		fmt.Fprintf(&b, "sealed interface %s {\n", a.name)
		fmt.Fprintf(&b, "    val value: String\n")
		// Data objects for each known value.
		for _, c := range a.constants {
			fmt.Fprintf(&b, "    @Serializable\n")
			fmt.Fprintf(&b, "    data object %s : %s {\n", a.shortName(c), a.name)
			fmt.Fprintf(&b, "        override val value = %q\n", c.value)
			b.WriteString("    }\n")
		}
		// Catch-all for forward compatibility.
		b.WriteString("    @Serializable\n")
		fmt.Fprintf(&b, "    data class Other(override val value: String) : %s\n", a.name)
		b.WriteString("}\n\n")
		// Serializer.
		fmt.Fprintf(&b, "object %sSerializer : KSerializer<%s> {\n", a.name, a.name)
		b.WriteString("    override val descriptor = PrimitiveSerialDescriptor(\"" + a.name + "\", PrimitiveKind.STRING)\n")
		fmt.Fprintf(&b, "    override fun serialize(encoder: Encoder, value: %s) = encoder.encodeString(value.value)\n", a.name)
		fmt.Fprintf(&b, "    override fun deserialize(decoder: Decoder): %s {\n", a.name)
		fmt.Fprintf(&b, "        val v = decoder.decodeString()\n")
		fmt.Fprintf(&b, "        return when (v) {\n")
		for _, c := range a.constants {
			fmt.Fprintf(&b, "            %q -> %s.%s\n", c.value, a.name, a.shortName(c))
		}
		fmt.Fprintf(&b, "            else -> %s.Other(v)\n", a.name)
		b.WriteString("        }\n")
		b.WriteString("    }\n")
		b.WriteString("}\n\n")
	}

	// Error codes.
	b.WriteString("object ErrorCodes {\n")
	for _, e := range d.cfg.errorCodes {
		fmt.Fprintf(&b, "    const val %s = %q\n", snakeToPascal(string(e.code)), e.code)
	}
	b.WriteString("}\n\n")

	// Structs: auto-discovered from route types and their transitive fields.
	kcStructs := d.discoverKotlinStructs()

	// Named slice type aliases.
	for _, ns := range collectNamedSlices(kcStructs) {
		ktElem, err := d.goTypeToKotlin(ns.Elem())
		if err != nil {
			return fmt.Errorf("%s: %w", ns.Name(), err)
		}
		fmt.Fprintf(&b, "typealias %s = List<%s>\n\n", ns.Name(), ktElem)
	}

	for _, ks := range kcStructs {
		if ks.comment != "" {
			fmt.Fprintf(&b, "// %s\n\n", ks.comment)
		}
		if err := d.emitKotlinStruct(&b, ks.t); err != nil {
			return err
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "Types.kt"), []byte(b.String()), 0o600)
}

// generateMarkdownDoc generates sdk/caic/API.md from the route table.
func (d *docRegistry) generateMarkdownDoc(outDir string) error {
	var b strings.Builder
	b.WriteString("# caic API Reference\n\n")
	b.WriteString("<!-- Code generated by gen-api-sdk; DO NOT EDIT. -->\n\n")
	b.WriteString("RESTful JSON API served at `/api/v1/`. SSE endpoints stream newline-delimited JSON events.\n\n")

	groups := docGroupRoutes(v1.Routes)

	// Route tables.
	for _, g := range groups {
		fmt.Fprintf(&b, "## %s\n\n", g.name)
		b.WriteString("| Method | Path | Description | Request | Response |\n")
		b.WriteString("|--------|------|-------------|---------|----------|\n")
		for i := range g.routes {
			r := &g.routes[i]
			req := ""
			if r.Req != nil {
				req = "`" + r.ReqName() + "`"
			}
			resp := "`" + r.RespName() + "`"
			if r.IsArray {
				resp = "`" + r.RespName() + "[]`"
			}
			if r.IsSSE {
				resp += " SSE"
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s |\n", r.Method, r.Path, r.Doc, req, resp)
		}
		b.WriteString("\n")
	}

	// Errors section.
	b.WriteString("## Errors\n\n")
	b.WriteString("All errors return:\n\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"error\": { \"code\": \"<CODE>\", \"message\": \"...\" },\n")
	b.WriteString("  \"details\": { ... }\n")
	b.WriteString("}\n")
	b.WriteString("```\n\n")
	b.WriteString("| HTTP | Code |\n")
	b.WriteString("|------|------|\n")
	for _, e := range d.cfg.errorCodes {
		fmt.Fprintf(&b, "| %d | `%s` |\n", e.status, e.code)
	}
	b.WriteString("\n")

	// Types section.
	b.WriteString("## Types\n\n")
	// Discover all API struct types reachable from Routes in
	// dependency order (leaves first).
	for _, t := range walkSDKTypes(routeSeedTypes()) {
		if err := d.writeDocType(&b, t); err != nil {
			return err
		}
	}

	return os.WriteFile(filepath.Join(outDir, "API.md"), []byte(b.String()), 0o600)
}

func (d *docRegistry) writeDocType(b *strings.Builder, t reflect.Type) error {
	fmt.Fprintf(b, "### %s\n\n", t.Name())
	if typeDoc := d.typeDoc[t.Name()]; typeDoc != "" {
		fmt.Fprintf(b, "%s\n\n", typeDoc)
	}
	b.WriteString("| Field | Type | Description | Required |\n")
	b.WriteString("|-------|------|-------------|----------|\n")
	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		optional := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero") || sf.Type.Kind() == reflect.Pointer
		typeName, err := d.cfg.goTypeToDoc(sf.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), jsonName, err)
		}
		req := "yes"
		if optional {
			req = ""
		}
		fieldDoc := ""
		if fdocs, ok := d.fieldDoc[t.Name()]; ok {
			fieldDoc = fdocs[sf.Name]
		}
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s |\n", jsonName, typeName, fieldDoc, req)
	}
	b.WriteString("\n")
	return nil
}

// goTypeToSwift maps a Go reflect.Type to its Swift type string.
func (d *docRegistry) goTypeToSwift(t reflect.Type) (string, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if m := d.cfg.lookupSpecial(t); m != nil {
		return m.swiftType, nil
	}

	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name(), nil
	}
	switch t.Kind() {
	case reflect.String:
		return "String", nil
	case reflect.Int, reflect.Int64, reflect.Uint64:
		return "Int", nil
	case reflect.Float64:
		return "Double", nil
	case reflect.Bool:
		return "Bool", nil
	case reflect.Slice:
		if isSDKPkg(t.PkgPath()) {
			return t.Name(), nil // Named slice.
		}
		e, err := d.goTypeToSwift(t.Elem())
		if err != nil {
			return "", err
		}
		return "[" + e + "]", nil
	case reflect.Map:
		k, err := d.goTypeToSwift(t.Key())
		if err != nil {
			return "", err
		}
		v, err := d.goTypeToSwift(t.Elem())
		if err != nil {
			return "", err
		}
		return "[" + k + ": " + v + "]", nil
	case reflect.Struct:
		return t.Name(), nil
	default:
		return "", fmt.Errorf("goTypeToSwift: unhandled kind %s for %s", t.Kind(), t)
	}
}

// emitSwiftStruct writes a public Codable struct to b.
func (d *docRegistry) emitSwiftStruct(b *strings.Builder, t reflect.Type) error {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatSwiftDoc(doc, ""))
	}
	name := t.Name()
	fmt.Fprintf(b, "public struct %s: Codable {\n", name)
	for sf := range t.Fields() {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts := parseJSONTag(tag)
		if jsonName == "" {
			jsonName = sf.Name
		}
		omit := slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero")
		isPtr := sf.Type.Kind() == reflect.Pointer
		swiftType, err := d.goTypeToSwift(sf.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, jsonName, err)
		}
		optional := isPtr || (omit && !isPtr)
		swiftName := swiftEscapeIdent(jsonName)

		if fdocs, ok := d.fieldDoc[name]; ok {
			if fdoc := fdocs[sf.Name]; fdoc != "" {
				b.WriteString(formatSwiftDoc(fdoc, "    "))
			}
		}
		if optional {
			fmt.Fprintf(b, "    public let %s: %s?\n", swiftName, swiftType)
		} else {
			fmt.Fprintf(b, "    public let %s: %s\n", swiftName, swiftType)
		}
	}
	b.WriteString("}\n")
	return nil
}

func (d *docRegistry) writeSwiftTypes(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\nimport Foundation\n\n")
	b.WriteString("/// ISO 8601 timestamp string (e.g. \"2026-04-13T12:00:00Z\").\n")
	b.WriteString("public typealias ISOTimestamp = String\n\n")

	// JSONValue: a Codable enum for arbitrary JSON (used for json.RawMessage / map[string]any fields).
	b.WriteString(`/// A Codable representation of an arbitrary JSON value.
public enum JSONValue: Codable, Equatable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case object([String: JSONValue])
    case array([JSONValue])
    case null

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null; return }
        if let v = try? c.decode(Bool.self) { self = .bool(v); return }
        if let v = try? c.decode(Double.self) { self = .number(v); return }
        if let v = try? c.decode(String.self) { self = .string(v); return }
        if let v = try? c.decode([String: JSONValue].self) { self = .object(v); return }
        if let v = try? c.decode([JSONValue].self) { self = .array(v); return }
        throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "Unknown JSON value"))
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
}

`)

	// Structs with static cases for exhaustive switching.
	for i := range d.aliases {
		a := &d.aliases[i]
		// Struct declaration.
		fmt.Fprintf(&b, "public struct %s: Codable, Equatable, Hashable {\n", a.name)
		b.WriteString("    public let value: String\n\n")
		b.WriteString("    public init(_ value: String) { self.value = value }\n\n")
		// Known values.
		for _, c := range a.constants {
			fmt.Fprintf(&b, "    public static let %s = %s(%q)\n", a.shortName(c), a.name, c.value)
		}
		b.WriteString("\n")
		// Catch-all for forward compatibility.
		fmt.Fprintf(&b, "    public static func other(_ value: String) -> %s { %s(value) }\n\n", a.name, a.name)
		// Codable: encode/decode as a plain string.
		b.WriteString("    public init(from decoder: Decoder) throws {\n")
		b.WriteString("        let c = try decoder.singleValueContainer()\n")
		b.WriteString("        value = try c.decode(String.self)\n")
		b.WriteString("    }\n\n")
		b.WriteString("    public func encode(to encoder: Encoder) throws {\n")
		b.WriteString("        var c = encoder.singleValueContainer()\n")
		b.WriteString("        try c.encode(value)\n")
		b.WriteString("    }\n")
		b.WriteString("}\n\n")
	}

	// Error codes.
	b.WriteString("public enum ErrorCodes {\n")
	for _, e := range d.cfg.errorCodes {
		fmt.Fprintf(&b, "    public static let %s = %q\n", snakeToCamel(string(e.code)), e.code)
	}
	b.WriteString("}\n\n")

	// Structs.
	swStructs := d.discoverSwiftStructs()

	// Named slice type aliases.
	for _, ns := range collectNamedSlices(swStructs) {
		if doc := d.typeDoc[ns.Name()]; doc != "" {
			b.WriteString(formatSwiftDoc(doc, ""))
		}
		swElem, err := d.goTypeToSwift(ns.Elem())
		if err != nil {
			return fmt.Errorf("%s: %w", ns.Name(), err)
		}
		fmt.Fprintf(&b, "public typealias %s = [%s]\n\n", ns.Name(), swElem)
	}

	for _, ks := range swStructs {
		if ks.comment != "" {
			fmt.Fprintf(&b, "// %s\n\n", ks.comment)
		}
		if err := d.emitSwiftStruct(&b, ks.t); err != nil {
			return err
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "Types.swift"), []byte(b.String()), 0o600)
}

// generateSwift generates Types.swift and ApiClient.swift in outDir.
func (d *docRegistry) generateSwift(outDir string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	if err := d.writeSwiftTypes(outDir); err != nil {
		return err
	}
	return writeSwiftClient(outDir)
}

// aliasInfo describes a Go named-string type and its constant values,
// extracted from Go source files by loadDocs.
type aliasInfo struct {
	name      string          // e.g. "Harness"
	file      string          // source filename, e.g. "types.go"
	constants []aliasConstant // enum values
}

// shortName returns the const name with the type prefix stripped
// (e.g. "HarnessClaude" → "Claude"). Used for Kotlin/Swift.
func (a aliasInfo) shortName(c aliasConstant) string { return strings.TrimPrefix(c.name, a.name) }

// aliasConstant is a single enum value for a string type alias.
type aliasConstant struct {
	name  string // const name from Go source, e.g. "HarnessClaude"
	value string // wire value, e.g. "claude"
}

// tsFieldBinding pairs a json field name with the TypeScript expression
// used to validate it in a discriminated union dispatch.
type tsFieldBinding struct {
	jsonName string
	expr     string
}

// sdkType wraps a reflect.Type with an optional section comment that
// appears before the struct in the generated output.
type sdkType struct {
	t       reflect.Type
	comment string // Emitted as `// comment` before the struct.
}

// kotlinField holds parsed information about a single struct field for Kotlin
// code generation.
type kotlinField struct {
	jsonName   string // JSON key from the struct tag.
	ktName     string // Kotlin property name (same as jsonName).
	ktType     string // Kotlin type (e.g. "String", "List<Foo>").
	nullable   bool   // Whether the field is T? = null.
	serialName string // Non-empty when @SerialName annotation is needed.
}

type docRouteGroup struct {
	name   string
	routes []v1.Route
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-api-sdk: %v\n", err)
		os.Exit(1)
	}
}

func mainImpl() error {
	// Output directories relative to go:generate CWD (backend/internal/server/api/v1/).
	const (
		sdkDir    = "../../../../../sdk/caic"
		tsDir     = sdkDir + "/ts/v1"
		kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/caic/sdk/v1"
		swiftDir  = sdkDir + "/swift/Sources/CaicSDK"
	)
	docs, err := loadDocs()
	if err != nil {
		return fmt.Errorf("loading docs: %w", err)
	}
	// Code generation configuration.
	docs.cfg = &genConfig{
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
			{api.CodeBadRequest, 400},
			{api.CodeNotFound, 404},
			{api.CodeConflict, 409},
			{api.CodeInternalError, 500},
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
