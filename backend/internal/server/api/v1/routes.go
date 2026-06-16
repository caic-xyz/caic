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
	Method      string       // HTTP method, e.g. "GET" or "POST"
	Path        string       // "/api/caic/v1/tasks/{id}/input"
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
// after "/api/caic/v1/", with the first letter uppercased.
// For example "/api/caic/v1/tasks/{id}/input" → "Tasks".
func (r *Route) CategoryName() string {
	// Strip "/api/caic/v1/" prefix, take the first segment.
	p := strings.TrimPrefix(r.Path, "/api/caic/v1/")
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
		Path:   "/api/caic/v1/server/config",
		Resp:   reflect.TypeFor[Config](),
	},
	{
		Name:   "getVersion",
		Doc:    "Returns the current server version and checks for available updates.",
		Method: "GET",
		Path:   "/api/caic/v1/server/version",
		Resp:   reflect.TypeFor[VersionResp](),
	},
	{
		Name:   "triggerUpdate",
		Doc:    "Triggers a background server auto-update to the latest release.",
		Method: "POST",
		Path:   "/api/caic/v1/server/update",
		Resp:   reflect.TypeFor[UpdateResp](),
	},
	{
		Name:   "getMe",
		Doc:    "Returns the authenticated user's profile.",
		Method: "GET",
		Path:   "/auth/me",
		Resp:   reflect.TypeFor[UserResp](),
	},
	{
		Name:   "logout",
		Doc:    "Invalidates the current session.",
		Method: "POST",
		Path:   "/auth/logout",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "getPreferences",
		Doc:    "Returns server and per-repository preferences.",
		Method: "GET",
		Path:   "/api/caic/v1/server/preferences",
		Resp:   reflect.TypeFor[PreferencesResp](),
	},
	{
		Name:   "updatePreferences",
		Doc:    "Updates server settings and preferences.",
		Method: "POST",
		Path:   "/api/caic/v1/server/preferences",
		Req:    reflect.TypeFor[UpdatePreferencesReq](),
		Resp:   reflect.TypeFor[PreferencesResp](),
	},
	{
		Name:   "listOAuthGrants",
		Doc:    "Lists the authenticated user's connected OAuth clients.",
		Method: "GET",
		Path:   "/api/caic/v1/oauth/grants",
		Resp:   reflect.TypeFor[OAuthGrantsResp](),
	},
	{
		Name:   "revokeOAuthGrant",
		Doc:    "Revokes one connected OAuth client grant for the authenticated user.",
		Method: "POST",
		Path:   "/api/caic/v1/oauth/grants/{grantID}/revoke",
		Req:    reflect.TypeFor[RevokeOAuthGrantReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:    "listHarnesses",
		Doc:     "Lists available coding agent harnesses.",
		Method:  "GET",
		Path:    "/api/caic/v1/server/harnesses",
		Resp:    reflect.TypeFor[HarnessInfo](),
		IsArray: true,
	},
	{
		Name:   "listCaches",
		Doc:    "Lists well-known cache configurations.",
		Method: "GET",
		Path:   "/api/caic/v1/server/caches",
		Resp:   reflect.TypeFor[WellKnownCachesResp](),
	},
	{
		Name:   "getCacheSizes",
		Doc:    "Returns the latest size snapshot for well-known caches.",
		Method: "GET",
		Path:   "/api/caic/v1/server/cache-sizes",
		Resp:   reflect.TypeFor[CacheSizesResp](),
	},
	{
		Name:    "listRepos",
		Doc:     "Lists all discovered repositories.",
		Method:  "GET",
		Path:    "/api/caic/v1/server/repos",
		Resp:    reflect.TypeFor[Repo](),
		IsArray: true,
	},
	{
		Name:   "cloneRepo",
		Doc:    "Clones a repository into the server's root directory.",
		Method: "POST",
		Path:   "/api/caic/v1/server/repos",
		Req:    reflect.TypeFor[CloneRepoReq](),
		Resp:   reflect.TypeFor[Repo](),
	},
	{
		Name:        "listRepoBranches",
		Doc:         "Lists branches for a repository.",
		Method:      "GET",
		Path:        "/api/caic/v1/server/repos/branches",
		Resp:        reflect.TypeFor[RepoBranchesResp](),
		QueryParams: []string{"repo"},
	},
	{
		Name:   "botFixCI",
		Doc:    "Creates a task to fix a failing CI pipeline.",
		Method: "POST",
		Path:   "/api/caic/v1/ci/fix-ci",
		Req:    reflect.TypeFor[BotFixCIReq](),
		Resp:   reflect.TypeFor[CreateTaskResp](),
	},
	{
		Name:   "botFixPR",
		Doc:    "Injects a CI fix command into an existing task's PR.",
		Method: "POST",
		Path:   "/api/caic/v1/ci/fix-pr",
		Req:    reflect.TypeFor[BotFixPRReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:    "listTasks",
		Doc:     "Returns all tasks.",
		Method:  "GET",
		Path:    "/api/caic/v1/tasks",
		Resp:    reflect.TypeFor[Task](),
		IsArray: true,
	},
	{
		Name:   "createTask",
		Doc:    "Creates and starts a new coding agent task.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks",
		Req:    reflect.TypeFor[CreateTaskReq](),
		Resp:   reflect.TypeFor[CreateTaskResp](),
	},
	{
		Name:   "taskRawEvents",
		Doc:    "Streams raw backend-specific task events via SSE.",
		Method: "GET",
		Path:   "/api/caic/v1/tasks/{id}/raw_events",
		Resp:   reflect.TypeFor[EventMessage](),
		IsSSE:  true,
	},
	{
		Name:   "taskEvents",
		Doc:    "Streams backend-neutral task events via SSE.",
		Method: "GET",
		Path:   "/api/caic/v1/tasks/{id}/events",
		Resp:   reflect.TypeFor[EventMessage](),
		IsSSE:  true,
	},
	{
		Name:   "sendInput",
		Doc:    "Sends user input to a running task.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/input",
		Req:    reflect.TypeFor[InputReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "restartTask",
		Doc:    "Restarts a completed or errored task with a new prompt.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/restart",
		Req:    reflect.TypeFor[RestartReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "clearContext",
		Doc:    "Clears context and restarts the agent session without a prompt.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/clear-context",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "compactContext",
		Doc:    "Sends a compact command to reduce the agent's context window usage.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/compact",
		Req:    reflect.TypeFor[CompactReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "stopTask",
		Doc:    "Requests graceful stop of a running task.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/stop",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "purgeTask",
		Doc:    "Permanently deletes a task and its runtime instance.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/purge",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "reviveTask",
		Doc:    "Reconnects to an orphaned task runtime instance.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/revive",
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:        "getTaskCILog",
		Doc:         "Returns the log tail of a failed CI check run.",
		Method:      "GET",
		Path:        "/api/caic/v1/ci/log/{id}",
		Resp:        reflect.TypeFor[CILogResp](),
		QueryParams: []string{"jobID"},
	},
	{
		Name:   "syncTask",
		Doc:    "Pushes task changes to the remote repository.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/sync",
		Req:    reflect.TypeFor[SyncReq](),
		Resp:   reflect.TypeFor[SyncResp](),
	},
	{
		Name:   "forkTask",
		Doc:    "Forks a task by snapshotting its runtime instance and creating a new task on a derived branch.",
		Method: "POST",
		Path:   "/api/caic/v1/tasks/{id}/fork",
		Req:    reflect.TypeFor[ForkTaskReq](),
		Resp:   reflect.TypeFor[CreateTaskResp](),
	},
	{
		Name:        "getTaskDiff",
		Doc:         "Returns the unified diff for a task's branch.",
		Method:      "GET",
		Path:        "/api/caic/v1/tasks/{id}/diff",
		Resp:        reflect.TypeFor[DiffResp](),
		QueryParams: []string{"path"},
	},
	{
		Name:   "getTaskProcesses",
		Doc:    "Returns the list of running processes inside the task's runtime instance.",
		Method: "GET",
		Path:   "/api/caic/v1/processes/{id}",
		Resp:   reflect.TypeFor[ProcessListResp](),
	},
	{
		Name:   "signalProcess",
		Doc:    "Sends SIGTERM or SIGKILL to a process inside the task's runtime instance.",
		Method: "POST",
		Path:   "/api/caic/v1/processes/{id}/{pid}/signal",
		Req:    reflect.TypeFor[SignalProcessReq](),
		Resp:   reflect.TypeFor[StatusResp](),
	},
	{
		Name:   "getTaskToolInput",
		Doc:    "Returns the full (untruncated) input for a tool call.",
		Method: "GET",
		Path:   "/api/caic/v1/tasks/{id}/tool/{toolUseID}",
		Resp:   reflect.TypeFor[TaskToolInputResp](),
	},
	{
		Name:   "globalTaskEvents",
		Doc:    "Streams task list updates for all tasks via SSE.",
		Method: "GET",
		Path:   "/api/caic/v1/tasks/events",
		Resp:   reflect.TypeFor[TaskListEvent](),
		IsSSE:  true,
	},
	{
		Name:   "globalUsageEvents",
		Doc:    "Streams usage quota updates via SSE.",
		Method: "GET",
		Path:   "/api/caic/v1/usage/events",
		Resp:   reflect.TypeFor[UsageResp](),
		IsSSE:  true,
	},
	{
		Name:   "getUsage",
		Doc:    "Returns current usage quota statistics.",
		Method: "GET",
		Path:   "/api/caic/v1/usage",
		Resp:   reflect.TypeFor[UsageResp](),
	},
	{
		Name:   "webFetch",
		Doc:    "Fetches a URL and returns its text content.",
		Method: "POST",
		Path:   "/api/caic/v1/web/fetch",
		Req:    reflect.TypeFor[WebFetchReq](),
		Resp:   reflect.TypeFor[WebFetchResp](),
	},
}
