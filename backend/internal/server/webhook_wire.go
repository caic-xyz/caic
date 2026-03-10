// Wire types for GitHub and GitLab webhook payloads.
package server

// githubCheckRunEvent is the payload for X-GitHub-Event: check_run.
type githubCheckRunEvent struct {
	Action   string `json:"action"`
	CheckRun struct {
		HeadSHA    string `json:"head_sha"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		ID         int64  `json:"id"`
	} `json:"check_run"`
	Repository struct {
		FullName string `json:"full_name"` // "owner/repo"
	} `json:"repository"`
}

// githubWorkflowRunEvent is the payload for X-GitHub-Event: workflow_run.
type githubWorkflowRunEvent struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		HeadSHA    string `json:"head_sha"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		ID         int64  `json:"id"`
	} `json:"workflow_run"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// gitlabPipelineEvent is the payload for X-Gitlab-Event: Pipeline Hook.
type gitlabPipelineEvent struct {
	ObjectAttributes struct {
		SHA    string `json:"sha"`
		Status string `json:"status"` // "success", "failed", "canceled", "skipped"
		ID     int64  `json:"id"`
	} `json:"object_attributes"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"` // "group/repo"
	} `json:"project"`
}
