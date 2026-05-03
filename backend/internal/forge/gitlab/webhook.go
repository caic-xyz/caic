// Payload types for GitLab webhook events.

package gitlab

// PipelineEvent is the payload for X-Gitlab-Event: Pipeline Hook.
type PipelineEvent struct {
	ObjectAttributes struct {
		SHA    string `json:"sha"`
		Status string `json:"status"` // "success", "failed", "canceled", "skipped"
		ID     int64  `json:"id"`
	} `json:"object_attributes"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"` // "group/repo"
	} `json:"project"`
}

// MergeRequestEvent is the payload for X-Gitlab-Event: Merge Request Hook.
type MergeRequestEvent struct {
	ObjectAttributes struct {
		IID        int    `json:"iid"`
		State      string `json:"state"` // "opened", "closed", "merged"
		Merged     bool   `json:"merged"`
		Title      string `json:"title"`
		Body       string `json:"description"`
		HeadBranch string `json:"head_branch"`
		BaseBranch string `json:"base_branch"`
		HeadSHA    string `json:"head_sha"`
		URL        string `json:"url"`
	} `json:"object_attributes"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"` // "group/repo"
	} `json:"project"`
	Repository struct {
		FullName string `json:"full_name"` // "group/repo" (may be empty; use Project)
	} `json:"repository"`
}
