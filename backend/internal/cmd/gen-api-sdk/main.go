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

	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/maruel/ksid"
)

// Output directories relative to go:generate CWD (backend/internal/server/dto/v1/).
const (
	sdkDir    = "../../../../../sdk"
	tsDir     = sdkDir + "/ts/v1"
	kotlinDir = sdkDir + "/kotlin/src/main/kotlin/com/caic/sdk/v1"
	swiftDir  = sdkDir + "/swift/Sources/CaicSDK"
)

var pathParamRe = regexp.MustCompile(`\{(\w+)\}`)

// skipErrorResponse is the skip set for TS generation, which emits
// dto.ErrorResponse as `any` instead of a generated struct.
var skipErrorResponse = map[reflect.Type]struct{}{
	reflect.TypeFor[dto.ErrorResponse](): {},
}

// Error code constants.
// docErrorCodes maps dto error codes to their HTTP status for API doc generation.
var docErrorCodes = []struct {
	code   dto.ErrorCode
	status int
}{
	{dto.CodeBadRequest, 400},
	{dto.CodeNotFound, 404},
	{dto.CodeConflict, 409},
	{dto.CodeInternalError, 500},
}

var kotlinErrorCodes = []aliasConstant{
	{"BadRequest", string(dto.CodeBadRequest)},
	{"NotFound", string(dto.CodeNotFound)},
	{"Conflict", string(dto.CodeConflict)},
	{"InternalError", string(dto.CodeInternalError)},
}

// kotlinSectionComments maps type names to section comments emitted before
// the struct in the generated output.
var kotlinSectionComments = map[string]string{
	"EventMessage": "Backend-neutral event types",
}

// Type identity values for special-case mapping in goTypeToKotlin.
var (
	jsonRawMessageType = reflect.TypeFor[json.RawMessage]()
	ksidIDType         = reflect.TypeFor[ksid.ID]()
	diffStatType       = reflect.TypeFor[v1.DiffStat]()
	mapStringAnyType   = reflect.TypeFor[map[string]any]()
	timeType           = reflect.TypeFor[time.Time]()
)

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

// swiftErrorCodes mirrors kotlinErrorCodes for Swift output.
var swiftErrorCodes = []aliasConstant{
	{"badRequest", string(dto.CodeBadRequest)},
	{"notFound", string(dto.CodeNotFound)},
	{"conflict", string(dto.CodeConflict)},
	{"internalError", string(dto.CodeInternalError)},
}

// swiftSectionComments mirrors kotlinSectionComments for Swift output.
var swiftSectionComments = map[string]string{
	"EventMessage": "Backend-neutral event types",
}

// isSDKPkg reports whether pkgPath is dto or dto/v1 — the two packages
// whose struct types are emitted into the generated SDK.
func isSDKPkg(pkgPath string) bool {
	return pkgPath == reflect.TypeFor[v1.StatusResp]().PkgPath() ||
		pkgPath == reflect.TypeFor[dto.ErrorResponse]().PkgPath()
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
// (leaves first), returning only dto or dto/v1 struct types. Types in skip
// are excluded entirely (neither emitted nor traversed).
func walkSDKTypes(seeds []reflect.Type, skip map[reflect.Type]struct{}) []reflect.Type {
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
		if _, ok := skip[t]; ok {
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		for i := range t.NumField() {
			walk(t.Field(i).Type)
		}
		order = append(order, t)
	}

	for _, t := range seeds {
		walk(t)
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
	fmt.Fprintf(b, "export type %s = string;\n", a.name)
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
func tsPrimitiveValidator(t reflect.Type, pathExpr, pathLit string) string {
	if t == timeType {
		return fmt.Sprintf("asString(%s, %q) as ISOTimestamp", pathExpr, pathLit)
	}
	if t == ksidIDType {
		return fmt.Sprintf("asString(%s, %q)", pathExpr, pathLit)
	}
	if t == jsonRawMessageType {
		return pathExpr // any — no validation
	}
	switch t.Kind() {
	case reflect.String:
		return fmt.Sprintf("asString(%s, %q)", pathExpr, pathLit)
	case reflect.Int, reflect.Int64, reflect.Uint64, reflect.Float64:
		return fmt.Sprintf("asNumber(%s, %q)", pathExpr, pathLit)
	case reflect.Bool:
		return fmt.Sprintf("asBoolean(%s, %q)", pathExpr, pathLit)
	default:
		return pathExpr
	}
}

// tsElemValidatorFunc returns a function expression (v: unknown) => validated
// for use as the element validator callback in validateArray/validateRecord.
func tsElemValidatorFunc(t reflect.Type, pathLit string) string {
	if t.Kind() == reflect.Pointer {
		return tsElemValidatorFunc(t.Elem(), pathLit)
	}
	if t.Kind() == reflect.Struct && isSDKPkg(t.PkgPath()) {
		return "validate" + t.Name()
	}
	return "(v) => " + tsPrimitiveValidator(t, "v", pathLit)
}

// discoverSSEStructs returns the structs reachable from the SSE event types
// (EventMessage, TaskListEvent, UsageResp) in dependency order.
func discoverSSEStructs() []kotlinStruct {
	seeds := []reflect.Type{
		reflect.TypeFor[v1.EventMessage](),
		reflect.TypeFor[v1.TaskListEvent](),
		reflect.TypeFor[v1.UsageResp](),
	}
	order := walkSDKTypes(seeds, nil)
	result := make([]kotlinStruct, len(order))
	for i, t := range order {
		result[i] = kotlinStruct{t: t}
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

// discoverKotlinStructs walks the dto struct types reachable from route
// types and returns them in dependency order (leaves first).
func discoverKotlinStructs() []kotlinStruct {
	seeds := append(routeSeedTypes(), reflect.TypeFor[dto.ErrorResponse]())
	order := walkSDKTypes(seeds, nil)
	result := make([]kotlinStruct, len(order))
	for i, t := range order {
		result[i] = kotlinStruct{t: t, comment: kotlinSectionComments[t.Name()]}
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

// discoverDocTypes returns all dto struct types reachable from Routes in
// dependency order (leaves first).
func discoverDocTypes() []reflect.Type {
	return walkSDKTypes(routeSeedTypes(), nil)
}

// goTypeToDoc maps a Go reflect.Type to a TypeScript-style string for docs.
func goTypeToDoc(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return goTypeToDoc(t.Elem())
	}
	switch t {
	case diffStatType:
		return "DiffFileStat[]"
	case ksidIDType:
		return "string"
	case timeType:
		return "ISOTimestamp"
	case jsonRawMessageType:
		return "object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int64, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice:
		return goTypeToDoc(t.Elem()) + "[]"
	case reflect.Struct:
		return t.Name()
	case reflect.Map:
		return "Record<string, unknown>"
	default:
		return t.Name()
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

// discoverSwiftStructs walks the dto struct types reachable from route types
// and returns them in dependency order, annotated with Swift section comments.
func discoverSwiftStructs() []kotlinStruct {
	seeds := append(routeSeedTypes(), reflect.TypeFor[dto.ErrorResponse]())
	order := walkSDKTypes(seeds, nil)
	result := make([]kotlinStruct, len(order))
	for i, t := range order {
		result[i] = kotlinStruct{t: t, comment: swiftSectionComments[t.Name()]}
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

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// docRegistry holds parsed documentation extracted from Go source files.
type docRegistry struct {
	typeDoc    map[string]string            // Go type name → doc comment text
	typeFile   map[string]string            // Go type name → source filename (e.g. "events.go")
	fieldDoc   map[string]map[string]string // Go type name → Go field name → doc comment text
	aliases    []aliasInfo                  // named-string types with their constants
	aliasNames map[string]struct{}          // set of alias type names for all target languages
}

// discoverTSStructs walks route types, skipping ErrorResponse, and annotates
// each struct with its source file for section grouping.
func (d *docRegistry) discoverTSStructs() []kotlinStruct {
	order := walkSDKTypes(routeSeedTypes(), skipErrorResponse)
	result := make([]kotlinStruct, len(order))
	for i, t := range order {
		result[i] = kotlinStruct{t: t, comment: d.typeFile[t.Name()]}
	}
	return result
}

// goTypeToTS maps a Go reflect.Type to its TypeScript type string.
func (d *docRegistry) goTypeToTS(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t {
	case jsonRawMessageType:
		return "any /* json.RawMessage */"
	case ksidIDType:
		return "string"
	case timeType:
		return "ISOTimestamp"
	case diffStatType:
		return "DiffFileStat[]"
	case mapStringAnyType:
		return "{ [key: string]: any /* json.RawMessage */}"
	case reflect.TypeFor[map[string]bool]():
		return "{ [key: string]: boolean}"
	case reflect.TypeFor[map[string]string]():
		return "{ [key: string]: string}"
	}

	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name()
	}

	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int:
		return "number /* int */"
	case reflect.Int64:
		return "number /* int64 */"
	case reflect.Uint64:
		return "number /* uint64 */"
	case reflect.Float64:
		return "number /* float64 */"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice:
		return d.goTypeToTS(t.Elem()) + "[]"
	case reflect.Map:
		return "{ [key: " + d.goTypeToTS(t.Key()) + "]: " + d.goTypeToTS(t.Elem()) + "}"
	case reflect.Struct:
		return t.Name()
	default:
		return "any"
	}
}

// emitTSStruct writes a TypeScript interface to b.
func (d *docRegistry) emitTSStruct(b *strings.Builder, t reflect.Type) {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatBlockDoc(doc, ""))
	}
	name := t.Name()
	fmt.Fprintf(b, "export interface %s {\n", name)
	for i := range t.NumField() {
		sf := t.Field(i)
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
		tsType := d.goTypeToTS(sf.Type)

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
}

// generateTSTypes generates sdk/ts/v1/types.gen.ts from the Go DTO structs.
func (d *docRegistry) generateTSTypes(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n")
	b.WriteString("/** ISO 8601 timestamp string (e.g. \"2026-04-13T12:00:00Z\"). */\n")
	b.WriteString("export type ISOTimestamp = string & { readonly __brand: \"ISOTimestamp\" };\n\n")

	allStructs := d.discoverTSStructs()

	// Group structs by source file for section headers.
	structsBySource := map[string][]kotlinStruct{}
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
			d.emitTSStruct(&b, ks.t)
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

	// DiffStat is a named slice type.
	b.WriteString("/**\n")
	b.WriteString(" * DiffStat summarises the changes in a branch relative to its base.\n")
	b.WriteString(" */\n")
	b.WriteString("export type DiffStat = DiffFileStat[];\n\n")
	seen["DiffStat"] = struct{}{}

	// Structs from types.go (and any ungrouped) in walk order.
	for _, ks := range allStructs {
		name := ks.t.Name()
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		d.emitTSStruct(&b, ks.t)
		b.WriteString("\n")
	}

	// ErrorResponse and EmptyReq type aliases (from dto package, not v1).
	b.WriteString("/**\n")
	b.WriteString(" * EmptyReq is used for endpoints that take no request body.\n")
	b.WriteString(" */\n")
	b.WriteString("export type EmptyReq = any /* dto.EmptyReq */;\n")
	b.WriteString("/**\n")
	b.WriteString(" * ErrorResponse is the JSON envelope for error responses.\n")
	b.WriteString(" */\n")
	b.WriteString("export type ErrorResponse = any /* dto.ErrorResponse */;\n")

	return os.WriteFile(filepath.Join(outDir, "types.gen.ts"), []byte(b.String()), 0o600)
}

// tsFieldValidator returns a TypeScript expression that validates a value
// at the given pathExpr. pathLit is a string literal for error messages.
func (d *docRegistry) tsFieldValidator(t reflect.Type, pathExpr, pathLit string) string {
	if t.Kind() == reflect.Pointer {
		return d.tsFieldValidator(t.Elem(), pathExpr, pathLit)
	}
	if t == jsonRawMessageType {
		return tsPrimitiveValidator(t, pathExpr, pathLit)
	}
	if t.Kind() == reflect.Slice {
		elemFn := tsElemValidatorFunc(t.Elem(), pathLit+"[i]")
		elemTS := d.goTypeToTS(t.Elem())
		return fmt.Sprintf("validateArray(%s, %q, %s) as %s[]", pathExpr, pathLit, elemFn, elemTS)
	}

	if t.Kind() == reflect.Map {
		valFn := tsElemValidatorFunc(t.Elem(), pathLit+"[k]")
		keyTS := d.goTypeToTS(t.Key())
		valTS := d.goTypeToTS(t.Elem())
		return fmt.Sprintf("validateRecord(%s, %q, %s) as { [key: %s]: %s }", pathExpr, pathLit, valFn, keyTS, valTS)
	}
	if t.Kind() == reflect.Struct && isSDKPkg(t.PkgPath()) {
		return fmt.Sprintf("validate%s(%s)", t.Name(), pathExpr)
	}
	return tsPrimitiveValidator(t, pathExpr, pathLit)
}

// tsFieldOptValidator is like tsFieldValidator but wraps the result to
// return undefined when the value is null/undefined.
func (d *docRegistry) tsFieldOptValidator(t reflect.Type, pathExpr, pathLit string) string {
	inner := d.tsFieldValidator(t, pathExpr, pathLit)
	return fmt.Sprintf("(%s === undefined || %s === null ? undefined : %s)", pathExpr, pathExpr, inner)
}

// emitTSValidator writes a validateXxx(raw: unknown): Xxx function for a struct.
func (d *docRegistry) emitTSValidator(b *strings.Builder, t reflect.Type) {
	name := t.Name()
	fmt.Fprintf(b, "export function validate%s(raw: unknown): %s {\n", name, name)
	fmt.Fprintf(b, "  const obj = asObject(raw, %q);\n", name)
	fmt.Fprintf(b, "  return {\n")

	for i := range t.NumField() {
		sf := t.Field(i)
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
		if optional {
			fmt.Fprintf(b, "    %s: %s,\n", jsonName, d.tsFieldOptValidator(sf.Type, pathExpr, pathLit))
		} else {
			fmt.Fprintf(b, "    %s: %s,\n", jsonName, d.tsFieldValidator(sf.Type, pathExpr, pathLit))
		}
	}

	b.WriteString("  };\n")
	b.WriteString("}\n\n")
}

// generateTSValidate generates sdk/ts/v1/validate.gen.ts with runtime
// validators for SSE-relevant DTO structs.
func (d *docRegistry) generateTSValidate(outDir string) error {
	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n\n")
	b.WriteString("// Runtime schema validators for SSE payloads.\n")
	b.WriteString("// Each validator checks structural correctness at runtime and throws\n")
	b.WriteString("// TypeError on mismatch. Unknown kinds pass through for forward compat.\n\n")

	// Collect all types referenced by validators.
	needed := discoverSSEStructs()
	refTypes := map[string]struct{}{}
	for _, ks := range needed {
		refTypes[ks.t.Name()] = struct{}{}
	}
	refTypes["EventMessage"] = struct{}{}
	refTypes["TaskListEvent"] = struct{}{}
	refTypes["UsageResp"] = struct{}{}
	refTypes["ISOTimestamp"] = struct{}{}

	if len(refTypes) > 0 {
		sorted := make([]string, 0, len(refTypes))
		for name := range refTypes {
			sorted = append(sorted, name)
		}
		slices.Sort(sorted)
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
	discriminated := map[string]struct{}{"EventMessage": {}, "TaskListEvent": {}, "UsageResp": {}}
	emitted := map[string]struct{}{}

	for _, ks := range needed {
		name := ks.t.Name()
		if _, ok := emitted[name]; ok {
			continue
		}
		if _, ok := discriminated[name]; ok {
			continue
		}
		emitted[name] = struct{}{}
		d.emitTSValidator(&b, ks.t)
	}

	// EventMessage discriminated union validator.
	b.WriteString("export function validateEventMessage(raw: unknown): EventMessage {\n")
	b.WriteString("  const obj = asObject(raw, \"EventMessage\");\n")
	b.WriteString("  const result: EventMessage = {\n")
	b.WriteString("    kind: asString(obj[\"kind\"], \"EventMessage.kind\"),\n")
	b.WriteString("    ts: asNumber(obj[\"ts\"], \"EventMessage.ts\"),\n")
	b.WriteString("  };\n")
	b.WriteString("  switch (result.kind) {\n")

	// Build the dispatch table from EventMessage's fields.
	evMsgType := reflect.TypeFor[v1.EventMessage]()
	for i := range evMsgType.NumField() {
		sf := evMsgType.Field(i)
		jsonName, _ := parseJSONTag(sf.Tag.Get("json"))
		if jsonName == "kind" || jsonName == "ts" {
			continue
		}
		// Each field is a pointer to a struct; get the struct name.
		fieldType := sf.Type.Elem()
		kindValue := jsonName // The json name IS the kind value for most (e.g. "init", "text").
		// Special case: diffStat kind is "diffStat", userInput kind is "userInput", etc.
		// The field json names match the kind constants.
		fmt.Fprintf(&b, "    case %q:\n", kindValue)
		fmt.Fprintf(&b, "      result.%s = validate%s(obj[%q]);\n", jsonName, fieldType.Name(), jsonName)
		b.WriteString("      break;\n")
	}

	b.WriteString("    // Unknown kinds pass through.\n")
	b.WriteString("  }\n")
	b.WriteString("  return result;\n")
	b.WriteString("}\n\n")

	// TaskListEvent discriminated union validator.
	b.WriteString("export function validateTaskListEvent(raw: unknown): TaskListEvent {\n")
	b.WriteString("  const obj = asObject(raw, \"TaskListEvent\");\n")
	b.WriteString("  const kind = asString(obj[\"kind\"], \"TaskListEvent.kind\");\n")
	b.WriteString("  const result: TaskListEvent = { kind };\n")
	b.WriteString("  switch (kind) {\n")
	b.WriteString("    case \"snapshot\":\n")
	b.WriteString("      result.tasks = validateArray(obj[\"tasks\"], \"TaskListEvent.tasks\", validateTask) as Task[];\n")
	b.WriteString("      break;\n")
	b.WriteString("    case \"upsert\":\n")
	b.WriteString("      result.task = validateTask(obj[\"task\"]);\n")
	b.WriteString("      break;\n")
	b.WriteString("    case \"patch\":\n")
	b.WriteString("      result.patch = asObject(obj[\"patch\"], \"TaskListEvent.patch\");\n")
	b.WriteString("      break;\n")
	b.WriteString("    case \"delete\":\n")
	b.WriteString("      result.id = asString(obj[\"id\"], \"TaskListEvent.id\");\n")
	b.WriteString("      break;\n")
	b.WriteString("    case \"repos\":\n")
	b.WriteString("      result.repos = validateArray(obj[\"repos\"], \"TaskListEvent.repos\", validateRepo) as Repo[];\n")
	b.WriteString("      break;\n")
	b.WriteString("    case \"warning\":\n")
	b.WriteString("      result.warning = asString(obj[\"warning\"], \"TaskListEvent.warning\");\n")
	b.WriteString("      break;\n")
	b.WriteString("    // Unknown kinds pass through.\n")
	b.WriteString("  }\n")
	b.WriteString("  return result;\n")
	b.WriteString("}\n\n")

	// UsageResp validator.
	b.WriteString("export function validateUsageResp(raw: unknown): UsageResp {\n")
	b.WriteString("  const obj = asObject(raw, \"UsageResp\");\n")
	b.WriteString("  const result: UsageResp = {\n")
	b.WriteString("    local: validateLocalUsage(obj[\"local\"]),\n")
	b.WriteString("  };\n")
	b.WriteString("  if (obj[\"providers\"] !== undefined && obj[\"providers\"] !== null) {\n")
	b.WriteString("    result.providers = validateArray(obj[\"providers\"], \"UsageResp.providers\", validateProviderQuota) as ProviderQuota[];\n")
	b.WriteString("  }\n")
	b.WriteString("  return result;\n")
	b.WriteString("}\n")

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

	sorted := sortedKeys(types)

	var b strings.Builder
	b.WriteString("// Code generated by gen-api-sdk. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "import type { %s } from \"./types.gen\";\n", strings.Join(sorted, ", "))
	if len(validators) > 0 {
		sortedVal := sortedKeys(validators)
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

func (d *docRegistry) goTypeToKotlin(t reflect.Type) string {
	// Unwrap pointer — nullability is handled by the caller.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	// Special cases.
	switch t {
	case jsonRawMessageType:
		return "JsonElement"
	case ksidIDType:
		return "String"
	case timeType:
		return "Instant"
	case diffStatType:
		return "List<DiffFileStat>"
	case mapStringAnyType:
		return "Map<String, JsonElement>"
	}

	// Named string aliases (Harness, EventKind).
	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name()
	}

	switch t.Kind() {
	case reflect.String:
		return "String"
	case reflect.Int:
		return "Int"
	case reflect.Int64:
		return "Long"
	case reflect.Uint64:
		return "Long" // JSON numbers; unsigned semantics not enforced at wire level.
	case reflect.Float64:
		return "Double"
	case reflect.Bool:
		return "Boolean"
	case reflect.Slice:
		return "List<" + d.goTypeToKotlin(t.Elem()) + ">"
	case reflect.Map:
		return "Map<" + d.goTypeToKotlin(t.Key()) + ", " + d.goTypeToKotlin(t.Elem()) + ">"
	case reflect.Struct:
		return t.Name()
	default:
		return t.Name()
	}
}

// parseStructFields extracts kotlinField entries from a reflect.Type.
func (d *docRegistry) parseStructFields(t reflect.Type) []kotlinField {
	fields := make([]kotlinField, 0, t.NumField())
	for i := range t.NumField() {
		sf := t.Field(i)
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

		ktType := d.goTypeToKotlin(ft)
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
	return fields
}

// emitKotlinStruct writes a @Serializable data class to b.
func (d *docRegistry) emitKotlinStruct(b *strings.Builder, t reflect.Type) {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatBlockDoc(doc, ""))
	}
	fields := d.parseStructFields(t)
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
		return
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

	// Type aliases with companion objects.
	for i := range d.aliases {
		a := &d.aliases[i]
		fmt.Fprintf(&b, "typealias %s = String\n\n", a.name)
		fmt.Fprintf(&b, "object %s {\n", a.plural())
		for _, c := range a.constants {
			fmt.Fprintf(&b, "    const val %s: %s = %q\n", a.shortName(c), a.name, c.value)
		}
		b.WriteString("}\n\n")
	}

	// Error codes.
	b.WriteString("object ErrorCodes {\n")
	for _, c := range kotlinErrorCodes {
		fmt.Fprintf(&b, "    const val %s = %q\n", c.name, c.value)
	}
	b.WriteString("}\n\n")

	// Structs: auto-discovered from route types and their transitive fields.
	for _, ks := range discoverKotlinStructs() {
		if ks.comment != "" {
			fmt.Fprintf(&b, "// %s\n\n", ks.comment)
		}
		d.emitKotlinStruct(&b, ks.t)
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "Types.kt"), []byte(b.String()), 0o600)
}

// generateDoc generates sdk/API.md from the route table.
func (d *docRegistry) generateDoc(outDir string) error {
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
	for _, e := range docErrorCodes {
		fmt.Fprintf(&b, "| %d | `%s` |\n", e.status, e.code)
	}
	b.WriteString("\n")

	// Types section.
	b.WriteString("## Types\n\n")
	for _, t := range discoverDocTypes() {
		d.writeDocType(&b, t)
	}

	return os.WriteFile(filepath.Join(outDir, "API.md"), []byte(b.String()), 0o600)
}

func (d *docRegistry) writeDocType(b *strings.Builder, t reflect.Type) {
	fmt.Fprintf(b, "### %s\n\n", t.Name())
	if typeDoc := d.typeDoc[t.Name()]; typeDoc != "" {
		fmt.Fprintf(b, "%s\n\n", typeDoc)
	}
	b.WriteString("| Field | Type | Description | Required |\n")
	b.WriteString("|-------|------|-------------|----------|\n")
	for i := range t.NumField() {
		sf := t.Field(i)
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
		typeName := goTypeToDoc(sf.Type)
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
}

// goTypeToSwift maps a Go reflect.Type to its Swift type string.
func (d *docRegistry) goTypeToSwift(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case jsonRawMessageType:
		return "JSONValue"
	case ksidIDType:
		return "String"
	case timeType:
		return "ISOTimestamp"
	case diffStatType:
		return "[DiffFileStat]"
	case mapStringAnyType:
		return "[String: JSONValue]"
	}
	if _, ok := d.aliasNames[t.Name()]; ok {
		return t.Name()
	}
	switch t.Kind() {
	case reflect.String:
		return "String"
	case reflect.Int, reflect.Int64:
		return "Int"
	case reflect.Float64:
		return "Double"
	case reflect.Bool:
		return "Bool"
	case reflect.Slice:
		return "[" + d.goTypeToSwift(t.Elem()) + "]"
	case reflect.Map:
		return "[" + d.goTypeToSwift(t.Key()) + ": " + d.goTypeToSwift(t.Elem()) + "]"
	case reflect.Struct:
		return t.Name()
	default:
		return t.Name()
	}
}

// emitSwiftStruct writes a public Codable struct to b.
func (d *docRegistry) emitSwiftStruct(b *strings.Builder, t reflect.Type) {
	if doc := d.typeDoc[t.Name()]; doc != "" {
		b.WriteString(formatSwiftDoc(doc, ""))
	}
	name := t.Name()
	fmt.Fprintf(b, "public struct %s: Codable {\n", name)
	for i := range t.NumField() {
		sf := t.Field(i)
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
		swiftType := d.goTypeToSwift(sf.Type)
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

	// Type aliases with constant namespaces.
	for i := range d.aliases {
		a := &d.aliases[i]
		fmt.Fprintf(&b, "public typealias %s = String\n\n", a.name)
		fmt.Fprintf(&b, "public enum %s {\n", a.plural())
		for _, c := range a.constants {
			fmt.Fprintf(&b, "    public static let %s: %s = %q\n", a.shortName(c), a.name, c.value)
		}
		b.WriteString("}\n\n")
	}

	// Error codes.
	b.WriteString("public enum ErrorCodes {\n")
	for _, c := range swiftErrorCodes {
		fmt.Fprintf(&b, "    public static let %s = %q\n", c.name, c.value)
	}
	b.WriteString("}\n\n")

	// Structs.
	for _, ks := range discoverSwiftStructs() {
		if ks.comment != "" {
			fmt.Fprintf(&b, "// %s\n\n", ks.comment)
		}
		d.emitSwiftStruct(&b, ks.t)
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

// plural returns the plural form for the type's constant namespace.
func (a aliasInfo) plural() string {
	if strings.HasSuffix(a.name, "s") {
		return a.name + "es"
	}
	return a.name + "s"
}

// aliasConstant is a single enum value for a string type alias.
type aliasConstant struct {
	name  string // const name from Go source, e.g. "HarnessClaude"
	value string // wire value, e.g. "claude"
}

// kotlinStruct wraps a reflect.Type with an optional section comment that
// appears before the struct in the generated output.
type kotlinStruct struct {
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
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	docs, err := loadDocs()
	if err != nil {
		return fmt.Errorf("loading docs: %w", err)
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
	return docs.generateDoc(sdkDir)
}
