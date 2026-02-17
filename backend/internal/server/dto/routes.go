// API route declarations used by the code generator to produce a typed TS client.
package dto

// Route describes a single API endpoint for code generation.
type Route struct {
	Name     string // TS function name, e.g. "listRepos"
	Method   string // "GET" or "POST"
	Path     string // "/api/v1/tasks/{id}/input"
	ReqType  string // TS type name or "" for no body
	RespType string // TS type name
	IsArray  bool   // response is T[] not T
	IsSSE    bool   // SSE stream, not JSON
}

// Routes is the authoritative list of API endpoints. The gen-api-client
// tool reads this slice to generate the typed TypeScript client.
var Routes = []Route{
	{Name: "listHarnesses", Method: "GET", Path: "/api/v1/harnesses", RespType: "HarnessJSON", IsArray: true},
	{Name: "listRepos", Method: "GET", Path: "/api/v1/repos", RespType: "RepoJSON", IsArray: true},
	{Name: "listTasks", Method: "GET", Path: "/api/v1/tasks", RespType: "TaskJSON", IsArray: true},
	{Name: "createTask", Method: "POST", Path: "/api/v1/tasks", ReqType: "CreateTaskReq", RespType: "CreateTaskResp"},
	{Name: "taskRawEvents", Method: "GET", Path: "/api/v1/tasks/{id}/raw_events", IsSSE: true, RespType: "ClaudeEventMessage"},
	{Name: "taskEvents", Method: "GET", Path: "/api/v1/tasks/{id}/events", IsSSE: true, RespType: "EventMessage"},
	{Name: "sendInput", Method: "POST", Path: "/api/v1/tasks/{id}/input", ReqType: "InputReq", RespType: "StatusResp"},
	{Name: "restartTask", Method: "POST", Path: "/api/v1/tasks/{id}/restart", ReqType: "RestartReq", RespType: "StatusResp"},
	{Name: "terminateTask", Method: "POST", Path: "/api/v1/tasks/{id}/terminate", RespType: "StatusResp"},
	{Name: "syncTask", Method: "POST", Path: "/api/v1/tasks/{id}/sync", ReqType: "SyncReq", RespType: "SyncResp"},
	{Name: "getUsage", Method: "GET", Path: "/api/v1/usage", RespType: "UsageResp"},
	{Name: "getVoiceToken", Method: "GET", Path: "/api/v1/voice/token", RespType: "VoiceTokenResp"},
}
