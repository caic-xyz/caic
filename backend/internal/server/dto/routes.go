// API route declarations used by the code generator to produce typed TS and Kotlin clients.
package dto

import (
	"reflect"
	"strings"
)

// Route describes a single API endpoint for code generation.
type Route struct {
	Name    string       // Function name, e.g. "listRepos"
	Method  string       // "GET" or "POST"
	Path    string       // "/api/v1/tasks/{id}/input"
	Req     reflect.Type // Request body type; nil for no body.
	Resp    reflect.Type // Response body type.
	IsArray bool         // response is T[] not T
	IsSSE   bool         // SSE stream, not JSON
}

// ReqName returns the request type name, or "" if Req is nil.
func (r *Route) ReqName() string {
	if r.Req == nil {
		return ""
	}
	return r.Req.Name()
}

// RespName returns the response type name.
func (r *Route) RespName() string {
	return r.Resp.Name()
}

// CategoryName returns the doc section derived from the first path segment
// after "/api/v1/", with the first letter uppercased.
// For example "/api/v1/tasks/{id}/input" → "Tasks".
func (r *Route) CategoryName() string {
	// Strip "/api/v1/" prefix, take the first segment.
	p := strings.TrimPrefix(r.Path, "/api/v1/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "Other"
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

// Routes is the authoritative list of API endpoints. The gen-api-sdk
// tool reads this slice to generate the typed TypeScript and Kotlin clients.
var Routes = []Route{
	{Name: "getConfig", Method: "GET", Path: "/api/v1/server/config", Resp: reflect.TypeFor[Config]()},
	{Name: "listHarnesses", Method: "GET", Path: "/api/v1/server/harnesses", Resp: reflect.TypeFor[HarnessInfo](), IsArray: true},
	{Name: "listRepos", Method: "GET", Path: "/api/v1/server/repos", Resp: reflect.TypeFor[Repo](), IsArray: true},
	{Name: "listTasks", Method: "GET", Path: "/api/v1/tasks", Resp: reflect.TypeFor[Task](), IsArray: true},
	{Name: "createTask", Method: "POST", Path: "/api/v1/tasks", Req: reflect.TypeFor[CreateTaskReq](), Resp: reflect.TypeFor[CreateTaskResp]()},
	{Name: "taskRawEvents", Method: "GET", Path: "/api/v1/tasks/{id}/raw_events", Resp: reflect.TypeFor[ClaudeEventMessage](), IsSSE: true},
	{Name: "taskEvents", Method: "GET", Path: "/api/v1/tasks/{id}/events", Resp: reflect.TypeFor[EventMessage](), IsSSE: true},
	{Name: "sendInput", Method: "POST", Path: "/api/v1/tasks/{id}/input", Req: reflect.TypeFor[InputReq](), Resp: reflect.TypeFor[StatusResp]()},
	{Name: "restartTask", Method: "POST", Path: "/api/v1/tasks/{id}/restart", Req: reflect.TypeFor[RestartReq](), Resp: reflect.TypeFor[StatusResp]()},
	{Name: "terminateTask", Method: "POST", Path: "/api/v1/tasks/{id}/terminate", Resp: reflect.TypeFor[StatusResp]()},
	{Name: "syncTask", Method: "POST", Path: "/api/v1/tasks/{id}/sync", Req: reflect.TypeFor[SyncReq](), Resp: reflect.TypeFor[SyncResp]()},
	{Name: "globalTaskEvents", Method: "GET", Path: "/api/v1/server/tasks/events", Resp: reflect.TypeFor[Task](), IsArray: true, IsSSE: true},
	{Name: "globalUsageEvents", Method: "GET", Path: "/api/v1/server/usage/events", Resp: reflect.TypeFor[UsageResp](), IsSSE: true},
	{Name: "getUsage", Method: "GET", Path: "/api/v1/usage", Resp: reflect.TypeFor[UsageResp]()},
	{Name: "getVoiceToken", Method: "GET", Path: "/api/v1/voice/token", Resp: reflect.TypeFor[VoiceTokenResp]()},
}
