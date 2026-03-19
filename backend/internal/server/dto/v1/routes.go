// API route declarations used by the code generator to produce typed TS and Kotlin clients.
package v1

import (
	"reflect"
	"strings"
)

// Route describes a single API endpoint for code generation.
type Route struct {
	Name        string       // Function name, e.g. "listRepos"
	Doc         string       // One-line description for SDK comments and docs.
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
	{
		Name:   "getConfig",
		Doc:    "Returns server capabilities and feature flags.",
		Method: "GET",
		Path:   "/api/v1/server/config",
		Resp:   reflect.TypeFor[Config](),
	},
	{
		Name:   "getMe",
		Doc:    "Returns the authenticated user's profile.",
		Method: "GET",
		Path:   "/api/v1/auth/me",
		Resp:   reflect.TypeFor[UserResp](),
	},
	{
		Name:   "logout",
		Doc:    "Invalidates the current session.",
		Method: "POST",
		Path:   "/api/v1/auth/logout",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "getPreferences",
		Doc:    "Returns server and per-repository preferences.",
		Method: "GET",
		Path:   "/api/v1/server/preferences",
		Resp:   reflect.TypeFor[PreferencesResp](),
	},
	{
		Name:   "updatePreferences",
		Doc:    "Updates server settings and preferences.",
		Method: "POST",
		Path:   "/api/v1/server/preferences",
		Req:    reflect.TypeFor[UpdatePreferencesReq](),
		Resp:   reflect.TypeFor[PreferencesResp](),
	},
	{
		Name:    "listHarnesses",
		Doc:     "Lists available coding agent harnesses.",
		Method:  "GET",
		Path:    "/api/v1/server/harnesses",
		Resp:    reflect.TypeFor[HarnessInfo](),
		IsArray: true,
	},
	{
		Name:   "listCaches",
		Doc:    "Lists well-known cache configurations.",
		Method: "GET",
		Path:   "/api/v1/server/caches",
		Resp:   reflect.TypeFor[WellKnownCachesResp](),
	},
	{
		Name:    "listRepos",
		Doc:     "Lists all discovered repositories.",
		Method:  "GET",
		Path:    "/api/v1/server/repos",
		Resp:    reflect.TypeFor[Repo](),
		IsArray: true,
	},
	{
		Name:   "cloneRepo",
		Doc:    "Clones a repository into the server's root directory.",
		Method: "POST",
		Path:   "/api/v1/server/repos",
		Req:    reflect.TypeFor[CloneRepoReq](),
		Resp:   reflect.TypeFor[Repo](),
	},
	{
		Name:        "listRepoBranches",
		Doc:         "Lists branches for a repository.",
		Method:      "GET",
		Path:        "/api/v1/server/repos/branches",
		Resp:        reflect.TypeFor[RepoBranchesResp](),
		QueryParams: []string{"repo"},
	},
	{
		Name:   "botFixCI",
		Doc:    "Creates a task to fix a failing CI pipeline.",
		Method: "POST",
		Path:   "/api/v1/bot/fix-ci",
		Req:    reflect.TypeFor[BotFixCIReq](),
		Resp:   reflect.TypeFor[CreateTaskResp](),
	},
	{
		Name:   "botFixPR",
		Doc:    "Injects a CI fix command into an existing task's PR.",
		Method: "POST",
		Path:   "/api/v1/bot/fix-pr",
		Req:    reflect.TypeFor[BotFixPRReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:    "listTasks",
		Doc:     "Returns all tasks.",
		Method:  "GET",
		Path:    "/api/v1/tasks",
		Resp:    reflect.TypeFor[Task](),
		IsArray: true,
	},
	{
		Name:   "createTask",
		Doc:    "Creates and starts a new coding agent task.",
		Method: "POST",
		Path:   "/api/v1/tasks",
		Req:    reflect.TypeFor[CreateTaskReq](),
		Resp:   reflect.TypeFor[CreateTaskResp](),
	},
	{
		Name:   "taskRawEvents",
		Doc:    "Streams raw backend-specific task events via SSE.",
		Method: "GET",
		Path:   "/api/v1/tasks/{id}/raw_events",
		Resp:   reflect.TypeFor[EventMessage](),
		IsSSE:  true,
	},
	{
		Name:   "taskEvents",
		Doc:    "Streams backend-neutral task events via SSE.",
		Method: "GET",
		Path:   "/api/v1/tasks/{id}/events",
		Resp:   reflect.TypeFor[EventMessage](),
		IsSSE:  true,
	},
	{
		Name:   "sendInput",
		Doc:    "Sends user input to a running task.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/input",
		Req:    reflect.TypeFor[InputReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "restartTask",
		Doc:    "Restarts a completed or errored task with a new prompt.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/restart",
		Req:    reflect.TypeFor[RestartReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "stopTask",
		Doc:    "Requests graceful stop of a running task.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/stop",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "purgeTask",
		Doc:    "Permanently deletes a task and its container.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/purge",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "reviveTask",
		Doc:    "Reconnects to an orphaned task container.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/revive",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:        "getTaskCILog",
		Doc:         "Returns the log tail of a failed CI check run.",
		Method:      "GET",
		Path:        "/api/v1/tasks/{id}/ci-log",
		Resp:        reflect.TypeFor[CILogResp](),
		QueryParams: []string{"jobID"},
	},
	{
		Name:   "syncTask",
		Doc:    "Pushes task changes to the remote repository.",
		Method: "POST",
		Path:   "/api/v1/tasks/{id}/sync",
		Req:    reflect.TypeFor[SyncReq](),
		Resp:   reflect.TypeFor[SyncResp](),
	},
	{
		Name:   "getTaskDiff",
		Doc:    "Returns the unified diff for a task's branch.",
		Method: "GET",
		Path:   "/api/v1/tasks/{id}/diff",
		Resp:   reflect.TypeFor[DiffResp](),
	},
	{
		Name:   "getTaskToolInput",
		Doc:    "Returns the full (untruncated) input for a tool call.",
		Method: "GET",
		Path:   "/api/v1/tasks/{id}/tool/{toolUseID}",
		Resp:   reflect.TypeFor[TaskToolInputResp](),
	},
	{
		Name:   "globalTaskEvents",
		Doc:    "Streams task list updates for all tasks via SSE.",
		Method: "GET",
		Path:   "/api/v1/server/tasks/events",
		Resp:   reflect.TypeFor[TaskListEvent](),
		IsSSE:  true,
	},
	{
		Name:   "globalUsageEvents",
		Doc:    "Streams usage quota updates via SSE.",
		Method: "GET",
		Path:   "/api/v1/server/usage/events",
		Resp:   reflect.TypeFor[UsageResp](),
		IsSSE:  true,
	},
	{
		Name:   "getUsage",
		Doc:    "Returns current usage quota statistics.",
		Method: "GET",
		Path:   "/api/v1/usage",
		Resp:   reflect.TypeFor[UsageResp](),
	},
	{
		Name:   "getVoiceToken",
		Doc:    "Returns a short-lived voice API token.",
		Method: "GET",
		Path:   "/api/v1/voice/token",
		Resp:   reflect.TypeFor[VoiceTokenResp](),
	},
	{
		Name:   "webFetch",
		Doc:    "Fetches a URL and returns its text content.",
		Method: "POST",
		Path:   "/api/v1/web/fetch",
		Req:    reflect.TypeFor[WebFetchReq](),
		Resp:   reflect.TypeFor[WebFetchResp](),
	},
	{
		Name:   "voiceRTCOffer",
		Doc:    "Exchanges a WebRTC SDP offer for an answer, opening a Gemini bridge session.",
		Method: "POST",
		Path:   "/api/v1/voice/rtc/offer",
		Req:    reflect.TypeFor[VoiceRTCOfferReq](),
		Resp:   reflect.TypeFor[VoiceRTCAnswerResp](),
	},
}
