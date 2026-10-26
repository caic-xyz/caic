// Shared MCP task/repository projections, caller-local timestamps, pagination, and bounded output.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	repodomain "github.com/caic-xyz/caic/backend/internal/repo"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/md/git"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"
)

// visibleTaskEntry resolves a task only when ordinary task access permits it.
func visibleTaskEntry(ctx context.Context, svc *taskService, id ksid.ID) (*taskmgr.Entry, bool) {
	entry, ok := svc.taskMgr.GetEntry(id)
	if !ok {
		return nil, false
	}
	if !taskAccessFromContext(ctx).canAccess(entry.Task()) {
		return nil, false
	}
	return entry, true
}

// inspectableTaskEntry resolves a task when its stable ID can be inspected.
func inspectableTaskEntry(ctx context.Context, svc *taskService, id ksid.ID) (*taskmgr.Entry, bool) {
	entry, ok := svc.taskMgr.GetEntry(id)
	if !ok {
		return nil, false
	}
	if !taskAccessFromContext(ctx).canInspect(entry.Task()) {
		return nil, false
	}
	return entry, true
}

// taskKeys returns visible task IDs in active-first, then ascending-ID order.
// The revision fingerprints membership and active status to invalidate stale task
// cursors; task fields that do not affect ordering do not change it.
func taskKeys(ctx context.Context, svc *taskService) (keys []mcpTaskKey, revision string) {
	access := taskAccessFromContext(ctx)
	keys = make([]mcpTaskKey, 0)
	for _, entry := range svc.taskMgr.Entries() {
		task := entry.Task()
		if !access.canAccess(task) {
			continue
		}
		state, err := apiconv.TaskState(task.GetState())
		if err != nil {
			svc.log.ErrorContext(ctx, "convert task state for MCP listing", "task", task.ID, "err", err)
			continue
		}
		keys = append(keys, mcpTaskKey{ID: task.ID, Active: taskStateActive(state)})
	}
	slices.SortFunc(keys, func(a, b mcpTaskKey) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return a.ID.Compare(b.ID)
	})
	hash := sha256.New()
	var idBytes [8]byte
	for _, key := range keys {
		if key.Active {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		binary.BigEndian.PutUint64(idBytes[:], uint64(key.ID))
		_, _ = hash.Write(idBytes[:])
	}
	return keys, base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

// taskSummaries projects a page of visible tasks using one-based numbers offset
// by start. It rejects disappeared tasks or a changed ordering revision so a page
// cannot silently mix incompatible task-number assignments.
func taskSummaries(ctx context.Context, svc *taskService, keys []mcpTaskKey, start int, revision string) ([]mcpTaskSummary, error) {
	summaries := make([]mcpTaskSummary, len(keys))
	for i, key := range keys {
		entry, ok := visibleTaskEntry(ctx, svc, key.ID)
		if !ok {
			return nil, errors.New("task ordering changed while listing")
		}
		task, err := taskDTO(ctx, entry, svc.taskMgr, svc.checkouts, svc.authStore)
		if err != nil {
			return nil, err
		}
		summaries[i] = taskMCPSummary(start+i+1, &task)
	}
	_, currentRevision := taskKeys(ctx, svc)
	if currentRevision != revision {
		return nil, errors.New("task ordering changed while listing")
	}
	return summaries, nil
}

// taskMCPSummary builds the bounded task-list view, preferring reported model
// and effort over requested values and aggregating diff counts without paths.
func taskMCPSummary(number int, task *v1.Task) mcpTaskSummary {
	var additions int
	var deletions int
	for _, file := range task.DiffStat {
		additions += file.LinesAdded
		deletions += file.LinesDeleted
	}
	message := ""
	switch task.State {
	case v1.TaskStatePurged:
		message = task.Result
	case v1.TaskStateStopped:
		message = "container stopped"
	case v1.TaskStateCrashed, v1.TaskStateFailed:
		message = task.Error
	default:
	}
	model := task.ReportedModel
	if model == "" {
		model = task.RequestedModel
	}
	effort := task.ReportedEffort
	if effort == "" {
		effort = task.RequestedEffort
	}
	return mcpTaskSummary{
		TaskNumber:      number,
		TaskID:          task.ID.String(),
		Title:           truncateUTF8(taskTitle(task), maxMCPTaskTitle),
		State:           task.State,
		Harness:         task.Harness,
		Model:           truncateUTF8(model, maxTaskSummaryModel),
		Effort:          truncateUTF8(effort, maxTaskSummaryEffort),
		RuntimeName:     truncateUTF8(task.Runtime.RuntimeName, maxTaskSummaryRuntime),
		StatusMessage:   truncateUTF8(message, maxTaskSummaryMessage),
		ChangedFiles:    len(task.DiffStat),
		Additions:       additions,
		Deletions:       deletions,
		ForgePR:         task.ForgePR,
		CIStatus:        task.CIStatus,
		DurationSeconds: task.Duration,
		CostUSD:         task.CostUSD,
	}
}

// taskTitle returns the display title, falling back to the stable task ID.
func taskTitle(t *v1.Task) string {
	if t.Title != "" {
		return t.Title
	}
	return t.ID.String()
}

// taskNumberForID returns the one-based position of id in the supplied snapshot,
// or zero when the task is absent.
func taskNumberForID(taskList []v1.Task, id string) int {
	for i := range taskList {
		if taskList[i].ID.String() == id {
			return i + 1
		}
	}
	return 0
}

var (
	// errMCPRepositoryListChanged reports a checkout removed during page projection.
	errMCPRepositoryListChanged = errors.New("repository list changed while paging")
	// errMCPRepositoryPathTooLong rejects paths that cannot fit the MCP path budget.
	errMCPRepositoryPathTooLong = errors.New("repository path exceeds the 512-byte MCP limit; shorten the configured path")
	// errMCPRepositoryPathInvalidUTF8 rejects paths that JSON cannot preserve exactly.
	errMCPRepositoryPathInvalidUTF8 = errors.New("repository path is not valid UTF-8; rename the configured path")
)

// mcpRepoSummary is the compact repository view shared by tools and resources.
type mcpRepoSummary struct {
	Path       string        `json:"path"                jsonschema_description:"Repository path used by task_create"`
	BaseBranch v1.BranchInfo `json:"baseBranch"          jsonschema_description:"Configured base branch"`
	Forge      v1.Forge      `json:"forge,omitempty"     jsonschema_description:"Repository forge provider"`
	RemoteURL  string        `json:"remoteURL,omitempty" jsonschema_description:"HTTPS remote URL"`
	CI         v1.CIStatus   `json:"ci,omitempty"        jsonschema_description:"Aggregate continuous integration status"`
}

// repositoryPaths returns configured checkout paths in ascending lexical order.
func repositoryPaths(config *serverHandlers) []string {
	var paths []string
	for checkout := range config.checkouts.Checkouts() {
		paths = append(paths, checkout.RelPath)
	}
	slices.Sort(paths)
	return paths
}

// repositorySummaries projects paths in their supplied order, reporting whether
// any display fields were shortened. Missing checkouts invalidate the page.
func repositorySummaries(config *serverHandlers, paths []string) (summaries []mcpRepoSummary, fieldsTruncated bool, err error) {
	summaries = make([]mcpRepoSummary, len(paths))
	for i, path := range paths {
		checkout, ok := config.checkouts.Checkout(path)
		if !ok {
			return nil, false, errMCPRepositoryListChanged
		}
		summary, truncated, summaryErr := repositorySummary(config, checkout)
		if summaryErr != nil {
			return nil, false, summaryErr
		}
		summaries[i] = summary
		fieldsTruncated = fieldsTruncated || truncated
	}
	return summaries, fieldsTruncated, nil
}

// repositorySummary validates the identifying path and bounds display fields.
// Its boolean reports repaired UTF-8 or truncated branch/remote text.
func repositorySummary(config *serverHandlers, checkout *repodomain.Checkout) (mcpRepoSummary, bool, error) {
	if err := validateMCPRepositoryPath(checkout.RelPath); err != nil {
		return mcpRepoSummary{}, false, err
	}
	var remote string
	var forgeKind v1.Forge
	if checkout.Repository != nil {
		remote = checkout.Repository.Remote
		var err error
		forgeKind, err = apiconv.RepoForge(checkout.Repository.ForgeKind)
		if err != nil {
			config.log.Error("convert repository forge for MCP", "repo", checkout.RelPath, "err", err)
		}
	}
	var ciStatus v1.CIStatus
	if config.repoStatus != nil {
		if status, ok := config.repoStatus.StatusFor(checkout.RelPath); ok {
			var err error
			ciStatus, err = apiconv.CIStatus(status)
			if err != nil {
				config.log.Error("convert repository CI status for MCP", "repo", checkout.RelPath, "err", err)
			}
		}
	}
	baseBranch, baseBranchTruncated := truncateMCPRepoField(checkout.BaseBranch)
	baseBranchRemote, baseBranchRemoteTruncated := truncateMCPRepoField(checkout.BaseBranchRemote)
	remoteURL, remoteURLTruncated := truncateMCPRepoField(git.RemoteToHTTPS(remote))
	return mcpRepoSummary{
		Path:       checkout.RelPath,
		BaseBranch: v1.BranchInfo{Name: baseBranch, Remote: baseBranchRemote},
		Forge:      forgeKind,
		RemoteURL:  remoteURL,
		CI:         ciStatus,
	}, baseBranchTruncated || baseBranchRemoteTruncated || remoteURLTruncated, nil
}

// validateMCPRepositoryPath requires an identifying path that fits the byte
// budget and survives JSON encoding unchanged; paths are never truncated.
func validateMCPRepositoryPath(path string) error {
	if len(path) > maxMCPRepoField {
		return errMCPRepositoryPathTooLong
	}
	if !utf8.ValidString(path) {
		return errMCPRepositoryPathInvalidUTF8
	}
	return nil
}

// mcpKeyCursor encodes a fixed-size SHA-256 anchor for a repository path or
// resource URI, keeping cursor size independent of key length.
func mcpKeyCursor(key string) string {
	digest := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// decodeMCPKeyCursor accepts only a strict, unpadded base64url SHA-256 digest.
func decodeMCPKeyCursor(cursor string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	if len(cursor) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return digest, false
	}
	n, err := base64.RawURLEncoding.Strict().Decode(digest[:], []byte(cursor))
	return digest, err == nil && n == sha256.Size
}

// paginateMCPRepositories resumes after the hashed anchor in sorted paths.
// Insertions before the anchor are tolerated; malformed or missing anchors fail.
// pageSize must be positive, and an absent next cursor marks the final page.
func paginateMCPRepositories(paths []string, cursor string, pageSize int) (page []string, next string, err error) {
	start := 0
	if cursor != "" {
		anchor, ok := decodeMCPKeyCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		found := false
		for i := range paths {
			if sha256.Sum256([]byte(paths[i])) != anchor {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", errors.New("invalid cursor")
		}
	}
	if start >= len(paths) {
		return []string{}, "", nil
	}
	end := min(start+pageSize, len(paths))
	if end < len(paths) {
		next = mcpKeyCursor(paths[end-1])
	}
	return paths[start:end], next, nil
}

// mcpTaskCursor binds the last emitted task ID to a task-ordering revision.
type mcpTaskCursor struct {
	Revision string `json:"revision"`
	TaskID   string `json:"taskID"`
}

// mcpResourceKind selects the descriptor projection used for a resource key.
type mcpResourceKind uint8

const (
	mcpResourceStatic mcpResourceKind = iota
	mcpResourceRepo
	mcpResourceTask
)

// mcpResourceKey identifies a resource without retaining its full descriptor.
type mcpResourceKey struct {
	URI    string
	Value  string
	TaskID ksid.ID
	Kind   mcpResourceKind
}

// paginateMCPResourceKeys pages URI-sorted keys after a hashed URI anchor.
// Earlier insertions are tolerated; a removed anchor requires restarting the
// listing without a cursor. An absent next cursor marks the final page.
func paginateMCPResourceKeys(keys []mcpResourceKey, cursor string) (page []mcpResourceKey, next string, err error) {
	start := 0
	if cursor != "" {
		anchor, ok := decodeMCPKeyCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		found := false
		for i := range keys {
			if sha256.Sum256([]byte(keys[i].URI)) != anchor {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", errors.New("resource list changed while paging; restart without a cursor")
		}
	}
	if start >= len(keys) {
		return []mcpResourceKey{}, "", nil
	}
	end := min(start+mcpResourcePageSize, len(keys))
	if end < len(keys) {
		next = mcpKeyCursor(keys[end-1].URI)
	}
	return keys[start:end], next, nil
}

// mcpTaskKey contains the task identity and active status used for list ordering.
type mcpTaskKey struct {
	ID     ksid.ID
	Active bool
}

// paginateMCPTaskKeys validates a revision-bound cursor and returns the next
// page with its zero-based offset for task numbering. pageSize must be positive;
// changed ordering, invalid anchors, or oversized cursors fail.
func paginateMCPTaskKeys(keys []mcpTaskKey, revision, cursor string, pageSize int) (page []mcpTaskKey, next string, start int, err error) {
	if cursor != "" {
		if len(cursor) > mcpTaskCursorMaxBytes {
			return nil, "", 0, errors.New("invalid cursor")
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if decodeErr != nil {
			return nil, "", 0, errors.New("invalid cursor")
		}
		var anchor mcpTaskCursor
		if json.Unmarshal(data, &anchor) != nil || anchor.Revision != revision {
			return nil, "", 0, errors.New("invalid cursor")
		}
		anchorID, parseErr := ksid.Parse(anchor.TaskID)
		if parseErr != nil {
			return nil, "", 0, errors.New("invalid cursor")
		}
		found := false
		for i := range keys {
			if keys[i].ID != anchorID {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", 0, errors.New("invalid cursor")
		}
	}
	if start >= len(keys) {
		return []mcpTaskKey{}, "", start, nil
	}
	end := min(start+pageSize, len(keys))
	if end < len(keys) {
		next, err = encodeMCPTaskCursor(revision, keys[end-1].ID)
		if err != nil {
			return nil, "", 0, err
		}
	}
	return keys[start:end], next, start, nil
}

// encodeMCPTaskCursor encodes the ordering revision and last emitted task ID
// as unpadded base64url JSON.
func encodeMCPTaskCursor(revision string, id ksid.ID) (string, error) {
	data, err := json.Marshal(mcpTaskCursor{Revision: revision, TaskID: id.String()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

const (
	// mcpListJSONMaxBytes bounds each compact structured list result. When a
	// count-based page would exceed it, the result ends at the last item that
	// fits and its cursor resumes at the following item.
	mcpListJSONMaxBytes     = 256 << 10
	mcpRepoPageSizeDefault  = 200
	mcpRepoPageSizeMax      = 200
	mcpResourceJSONMaxBytes = 256 << 10
	mcpResourcePageSize     = 100
	mcpServiceItemMaxCount  = 50
	mcpTextOutputMaxBytes   = 32 << 10
	mcpTaskPageSizeDefault  = 20
	mcpTaskPageSizeMax      = 50
	mcpTaskCursorMaxBytes   = 1024
	maxMCPSafetyIssues      = 50
	maxMCPRepoField         = 512
	maxTaskDetailPaths      = 50
	maxTaskSummaryEffort    = 64
	maxTaskSummaryMessage   = 512
	maxTaskSummaryModel     = 256
	maxTaskSummaryRuntime   = 128
	maxMCPTaskTitle         = 512
)

const mcpTruncatedMetaKey = "xyz.caic/truncated"

// truncateMCPRepoField repairs invalid UTF-8 and bounds a repository display
// field; the boolean reports any repair or truncation.
func truncateMCPRepoField(value string) (string, bool) {
	original := value
	value = strings.ToValidUTF8(value, "�")
	truncated := truncateUTF8(value, maxMCPRepoField)
	return truncated, truncated != original
}

// fitMCPJSONPage fits a leading item slice and its continuation cursor into the
// JSON byte budget. hasMore indicates later items beyond the supplied slice;
// cursorForCount must anchor the last retained item. outputForPage must satisfy
// fitMCPJSONPrefix's monotonic size contract. An unfit first item is an error.
func fitMCPJSONPage[T any](
	items []T,
	hasMore bool,
	maxBytes int,
	cursorForCount func(int) (string, error),
	outputForPage func([]T, string) any,
) (page []T, next string, err error) {
	if len(items) == 0 {
		return items, "", nil
	}
	pageFits := func(count int, includeCursor bool) (string, bool, error) {
		var cursor string
		var err error
		if includeCursor {
			cursor, err = cursorForCount(count)
			if err != nil {
				return "", false, err
			}
		}
		data, err := json.Marshal(outputForPage(items[:count], cursor))
		return cursor, len(data) <= maxBytes, err
	}

	if cursor, fits, err := pageFits(len(items), hasMore); err != nil || fits {
		return items, cursor, err
	}
	page, err = fitMCPJSONPrefix(items[:len(items)-1], maxBytes, func(page []T) (any, error) {
		if len(page) == 0 {
			return outputForPage(page, ""), nil
		}
		cursor, cursorErr := cursorForCount(len(page))
		return outputForPage(page, cursor), cursorErr
	})
	if err != nil {
		return nil, "", err
	}
	if len(page) == 0 {
		return nil, "", errors.New("first page item exceeds JSON response limit")
	}
	next, err = cursorForCount(len(page))
	return page, next, err
}

// fitMCPJSONPrefix returns the largest leading slice whose wrapped JSON fits.
// outputForPrefix must produce monotonically nondecreasing encoded sizes as the
// prefix grows so the binary search cannot skip a later, smaller representation.
func fitMCPJSONPrefix[T any](items []T, maxBytes int, outputForPrefix func([]T) (any, error)) ([]T, error) {
	best := -1
	low, high := 0, len(items)
	for low <= high {
		middle := low + (high-low)/2
		output, err := outputForPrefix(items[:middle])
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(output)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxBytes {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best < 0 {
		return nil, errors.New("empty MCP JSON response exceeds limit")
	}
	return items[:best], nil
}

// boundedTextToolResult caps tool text and marks shortened output in metadata.
func boundedTextToolResult(text string) mcp.ToolResult[mcp.TextOutput] {
	text, truncated := truncateMCPText(text)
	if !truncated {
		return mcp.TextToolResult(text)
	}
	result := mcp.TextToolResult(text)
	result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	return result
}

// truncateMCPText caps tool text including a truncation notice, cutting only at
// a UTF-8 rune boundary when the byte budget is exceeded.
func truncateMCPText(text string) (string, bool) {
	if len(text) <= mcpTextOutputMaxBytes {
		return text, false
	}
	const suffix = "\n\n[Output truncated. Use a more specific tool or resource to inspect the omitted data.]"
	end := mcpTextOutputMaxBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix, true
}

// boundedResourceJSON wraps JSON as a private, cacheable resource read.
// Values exceeding the byte budget are rejected rather than partially encoded.
func boundedResourceJSON(uri string, value any) (mcp.ResourcesReadResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return mcp.ResourcesReadResult{}, err
	}
	// TODO(observability): Record MCP resource response bytes and oversize
	// rejections by stable resource family here. Do not label metrics with full
	// resource URIs because task IDs and repository names are unbounded.
	if len(data) > mcpResourceJSONMaxBytes {
		return mcp.ResourcesReadResult{}, errors.New("resource exceeds the 256 KiB response limit; read a more specific resource URI")
	}
	return mcp.ResourcesReadResult{
		ResultType: mcp.ResultTypeComplete,
		Contents:   []mcp.ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}},
		TTLMS:      mcp.DefaultTTLMS,
		CacheScope: mcp.CacheScopePrivate,
	}, nil
}

// truncateMCPTaskTitle bounds a title and reports whether it was shortened.
func truncateMCPTaskTitle(title string) (string, bool) {
	truncated := truncateUTF8(title, maxMCPTaskTitle)
	return truncated, truncated != title
}

// truncateUTF8 bounds valid UTF-8 text by bytes, appending an ellipsis when
// shortened without splitting a rune. maxBytes must fit the ellipsis itself.
func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "…"
	end := maxBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}

// mcpRequestLocation resolves the caller's named timezone, using UTC for an
// empty, invalid, or process-local timezone name.
func mcpRequestLocation(meta *mcp.RequestMeta) *time.Location {
	if zone := meta.UserLocation.Timezone; zone != "" && zone != "Local" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// mcpUsageInLocation converts usage timestamps while cloning the provider and
// rate-limit slices so the shared response retains its original timestamps.
func mcpUsageInLocation(usage v1.UsageResp, loc *time.Location) v1.UsageResp {
	usage.Providers = slices.Clone(usage.Providers)
	for i := range usage.Providers {
		p := &usage.Providers[i]
		p.PricingTransitionAt = mcpTimeInLocation(p.PricingTransitionAt, loc)
		p.RateLimits = slices.Clone(p.RateLimits)
		for j := range p.RateLimits {
			p.RateLimits[j].ResetsAt = mcpTimeInLocation(p.RateLimits[j].ResetsAt, loc)
		}
	}
	return usage
}

// mcpTaskInLocation returns caller-local task timestamps and clones the CI
// checks before converting them, leaving the source snapshot unchanged.
func mcpTaskInLocation(source *v1.Task, loc *time.Location) v1.Task {
	task := *source
	task.CacheExpiresAt = mcpTimeInLocation(task.CacheExpiresAt, loc)
	task.StartedAt = mcpTimeInLocation(task.StartedAt, loc)
	task.StateUpdatedAt = mcpTimeInLocation(task.StateUpdatedAt, loc)
	task.TurnStartedAt = mcpTimeInLocation(task.TurnStartedAt, loc)
	task.RateLimit.ResetsAt = mcpTimeInLocation(task.RateLimit.ResetsAt, loc)
	task.CIChecks = slices.Clone(task.CIChecks)
	for i := range task.CIChecks {
		c := &task.CIChecks[i]
		c.QueuedAt = mcpTimeInLocation(c.QueuedAt, loc)
		c.StartedAt = mcpTimeInLocation(c.StartedAt, loc)
		c.CompletedAt = mcpTimeInLocation(c.CompletedAt, loc)
	}
	return task
}

// mcpNotificationsInLocation clones notification events before converting
// their timestamps, preserving the shared feed's timestamps.
func mcpNotificationsInLocation(events []serviceNotification, loc *time.Location) []serviceNotification {
	events = slices.Clone(events)
	for i := range events {
		events[i].OccurredAt = mcpTimeInLocation(events[i].OccurredAt, loc)
		events[i].ExpiresAt = mcpTimeInLocation(events[i].ExpiresAt, loc)
	}
	return events
}

// mcpTimeInLocation converts nonzero timestamps while preserving zero values.
func mcpTimeInLocation(t time.Time, loc *time.Location) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.In(loc)
}

// mcpSystemStatsInLocation clones the sample slice before converting its
// timestamps, leaving the shared sampler snapshot unchanged.
func mcpSystemStatsInLocation(stats v1.SystemStatsResp, loc *time.Location) v1.SystemStatsResp {
	stats.Samples = slices.Clone(stats.Samples)
	for i := range stats.Samples {
		stats.Samples[i].Time = mcpTimeInLocation(stats.Samples[i].Time, loc)
	}
	return stats
}
