// Route definition and generated client method emitters.

package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// routeDef describes a single API endpoint for generated clients and docs.
type routeDef struct {
	Name        string
	Doc         string
	Method      string
	Path        string
	Category    string
	Req         reflect.Type
	Resp        reflect.Type
	IsArray     bool
	IsSSE       bool
	QueryParams []string
}

func (r *routeDef) reqName() string {
	if r.Req == nil {
		return ""
	}
	return r.Req.Name()
}

func (r *routeDef) respName() string {
	return r.Resp.Name()
}

func (r *routeDef) categoryName() string {
	if r.Category != "" {
		return r.Category
	}
	p := strings.TrimPrefix(r.Path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "Other"
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

func (r *routeDef) writeTSJSONMethod(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	respType := r.respName()
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
		args = append(args, "req: "+r.reqName())
	}

	tsPath := buildTSPath(r.Path, params, r.QueryParams)

	if hasReq {
		fmt.Fprintf(b, "    %s: (%s): Promise<%s> => request<%s>(%q, %s, req),\n", r.Name, strings.Join(args, ", "), respType, respType, r.Method, tsPath)
	} else {
		fmt.Fprintf(b, "    %s: (%s): Promise<%s> => request<%s>(%q, %s),\n", r.Name, strings.Join(args, ", "), respType, respType, r.Method, tsPath)
	}
}

func (r *routeDef) writeTSSSEMethod(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params)+1)
	for _, p := range params {
		args = append(args, p+": string")
	}
	tsPath := buildTSPath(r.Path, params, nil)
	respName := r.respName()
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

func (r *routeDef) writeKotlinJSONFunc(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	respType := r.respName()
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
		args = append(args, "req: "+r.reqName())
	}

	ktPath := buildKotlinPath(r.Path, r.QueryParams)

	sig := strings.Join(args, ", ")
	if hasReq {
		fmt.Fprintf(b, "    suspend fun %s(%s): %s = request(%q, %s, json.encodeToString(req))\n", r.Name, sig, respType, r.Method, ktPath)
	} else {
		fmt.Fprintf(b, "    suspend fun %s(%s): %s = request(%q, %s)\n", r.Name, sig, respType, r.Method, ktPath)
	}
}

func (r *routeDef) writeKotlinSSEFunc(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatBlockDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params))
	for _, p := range params {
		args = append(args, p+": String")
	}
	ktPath := buildKotlinPath(r.Path, nil)
	respName := r.respName()
	fmt.Fprintf(b, "    fun %s(%s): Flow<%s> = sseFlow<%s>(%s)\n", r.Name, strings.Join(args, ", "), respName, respName, ktPath)
}

func (r *routeDef) writeKotlinReconnectingFunc(b *strings.Builder, params []string) {
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
		reconnectName, strings.Join(args, ", "), r.respName(), r.Name, strings.Join(callArgs, ", "))
}

func (r *routeDef) writeSwiftJSONFunc(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatSwiftDoc(r.Doc, "    "))
	}
	respType := r.respName()
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
		args = append(args, "req: "+r.reqName())
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

func (r *routeDef) writeSwiftSSEFunc(b *strings.Builder, params []string) {
	if r.Doc != "" {
		b.WriteString(formatSwiftDoc(r.Doc, "    "))
	}
	args := make([]string, 0, len(params))
	for _, p := range params {
		args = append(args, p+": String")
	}
	swiftPath := buildSwiftPath(r.Path, nil)
	respName := r.respName()
	fmt.Fprintf(b, "    public func %s(%s) -> AsyncThrowingStream<%s, Error> {\n", r.Name, strings.Join(args, ", "), respName)
	fmt.Fprintf(b, "        sseStream(path: %s)\n", swiftPath)
	b.WriteString("    }\n")
}

func (r *routeDef) writeSwiftReconnectingFunc(b *strings.Builder, params []string) {
	allParams := slices.Concat(params, r.QueryParams)
	args := make([]string, 0, len(allParams))
	callArgs := make([]string, 0, len(allParams))
	for _, p := range allParams {
		args = append(args, p+": String")
		callArgs = append(callArgs, p+": "+p)
	}
	reconnectName := r.Name + "Reconnecting"
	respName := r.respName()
	fmt.Fprintf(b, "    public func %s(%s) -> AsyncThrowingStream<%s, Error> {\n",
		reconnectName, strings.Join(args, ", "), respName)
	fmt.Fprintf(b, "        reconnectingStream { self.%s(%s) }\n", r.Name, strings.Join(callArgs, ", "))
	b.WriteString("    }\n")
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
