// API route declarations used by the code generator to produce typed TS and Kotlin clients.
package v1

import (
	"reflect"
	"strings"
)

// Route describes a single API endpoint for code generation.
type Route struct {
	Name        string       // Function name, e.g. "listRepos"
	Method      string       // "GET" or "POST"
	Path        string       // "/api/v1/tasks/{id}/input"
	Req         reflect.Type // Request body type; nil for no body.
	Resp        reflect.Type // Response body type.
	IsArray     bool         // response is T[] not T
	IsSSE       bool         // SSE stream, not JSON
	QueryParams []string     // Query parameter names (GET endpoints only).
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
	{Name: "getMe", Method: "GET", Path: "/api/v1/auth/me", Resp: reflect.TypeFor[UserResp]()},
	{Name: "logout", Method: "POST", Path: "/api/v1/auth/logout", Resp: reflect.TypeFor[StatusResp]()},
	{Name: "getPreferences", Method: "GET", Path: "/api/v1/server/preferences", Resp: reflect.TypeFor[PreferencesResp]()},
	{Name: "updatePreferences", Method: "POST", Path: "/api/v1/server/preferences", Req: reflect.TypeFor[UpdatePreferencesReq](), Resp: reflect.TypeFor[PreferencesResp]()},
	{Name: "listHarnesses", Method: "GET", Path: "/api/v1/server/harnesses", Resp: reflect.TypeFor[HarnessInfo](), IsArray: true},
	{Name: "listCaches", Method: "GET", Path: "/api/v1/server/caches", Resp: reflect.TypeFor[WellKnownCachesResp]()},
	{Name: "listRepos", Method: "GET", Path: "/api/v1/server/repos", Resp: reflect.TypeFor[Repo](), IsArray: true},
	{Name: "cloneRepo", Method: "POST", Path: "/api/v1/server/repos", Req: reflect.TypeFor[CloneRepoReq](), Resp: reflect.TypeFor[Repo]()},
	{Name: "listRepoBranches", Method: "GET", Path: "/api/v1/server/repos/branches", Resp: reflect.TypeFor[RepoBranchesResp](), QueryParams: []string{"repo"}},
	{Name: "botFixCI", Method: "POST", Path: "/api/v1/bot/fix-ci", Req: reflect.TypeFor[BotFixCIReq](), Resp: reflect.TypeFor[CreateTaskResp]()},
	{Name: "botFixPR", Method: "POST", Path: "/api/v1/bot/fix-pr", Req: reflect.TypeFor[BotFixPRReq](), Resp: reflect.TypeFor[StatusResp]()},
	{Name: "listTasks", Method: "GET", Path: "/api/v1/tasks", Resp: reflect.TypeFor[Task](), IsArray: true},
	{Name: "createTask", Method: "POST", Path: "/api/v1/tasks", Req: reflect.TypeFor[CreateTaskReq](), Resp: reflect.TypeFor[CreateTaskResp]()},
	{Name: "taskRawEvents", Method: "GET", Path: "/api/v1/tasks/{id}/raw_events", Resp: reflect.TypeFor[EventMessage](), IsSSE: true},
	{Name: "taskEvents", Method: "GET", Path: "/api/v1/tasks/{id}/events", Resp: reflect.TypeFor[EventMessage](), IsSSE: true},
	{Name: "sendInput", Method: "POST", Path: "/api/v1/tasks/{id}/input", Req: reflect.TypeFor[InputReq](), Resp: reflect.TypeFor[StatusResp]()},
	{Name: "restartTask", Method: "POST", Path: "/api/v1/tasks/{id}/restart", Req: reflect.TypeFor[RestartReq](), Resp: reflect.TypeFor[StatusResp]()},
	{Name: "stopTask", Method: "POST", Path: "/api/v1/tasks/{id}/stop", Resp: reflect.TypeFor[StatusResp]()},
	{Name: "purgeTask", Method: "POST", Path: "/api/v1/tasks/{id}/purge", Resp: reflect.TypeFor[StatusResp]()},
	{Name: "reviveTask", Method: "POST", Path: "/api/v1/tasks/{id}/revive", Resp: reflect.TypeFor[StatusResp]()},
	{Name: "getTaskCILog", Method: "GET", Path: "/api/v1/tasks/{id}/ci-log", Resp: reflect.TypeFor[CILogResp](), QueryParams: []string{"jobID"}},
	{Name: "syncTask", Method: "POST", Path: "/api/v1/tasks/{id}/sync", Req: reflect.TypeFor[SyncReq](), Resp: reflect.TypeFor[SyncResp]()},
	{Name: "getTaskDiff", Method: "GET", Path: "/api/v1/tasks/{id}/diff", Resp: reflect.TypeFor[DiffResp]()},
	{Name: "getTaskToolInput", Method: "GET", Path: "/api/v1/tasks/{id}/tool/{toolUseID}", Resp: reflect.TypeFor[TaskToolInputResp]()},
	{Name: "globalTaskEvents", Method: "GET", Path: "/api/v1/server/tasks/events", Resp: reflect.TypeFor[TaskListEvent](), IsSSE: true},
	{Name: "globalUsageEvents", Method: "GET", Path: "/api/v1/server/usage/events", Resp: reflect.TypeFor[UsageResp](), IsSSE: true},
	{Name: "getUsage", Method: "GET", Path: "/api/v1/usage", Resp: reflect.TypeFor[UsageResp]()},
	{Name: "getVoiceToken", Method: "GET", Path: "/api/v1/voice/token", Resp: reflect.TypeFor[VoiceTokenResp]()},
	{Name: "webFetch", Method: "POST", Path: "/api/v1/web/fetch", Req: reflect.TypeFor[WebFetchReq](), Resp: reflect.TypeFor[WebFetchResp]()},
}
