// Server startup: New() constructor, container adoption, and background maintenance.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/maruel/ksid"
)

// repoDiscoveryDepth is the maximum directory levels below absRoot that
// DiscoverRepos scans for git repositories, both at startup and during
// background polling.
const repoDiscoveryDepth = 3

// repoInitResult holds the outcome of initialising a single newly-discovered
// repository.
type repoInitResult struct {
	info   repoInfo
	runner *task.Runner
}

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
//
// Startup sequence:
//  1. Initialize container client (instant).
//  2. Parallel I/O phase: discover repos, load purged task logs, and list
//     containers concurrently.
//  3. Runner init phase: create a Runner per repo with container and agent backends
//     (runs parallel within after repos are discovered).
//  4. Adopt containers using pre-fetched list and logs. If a container's relay
//     is alive, auto-attach to resume streaming.
func New(ctx context.Context, rootDir string, cfg *Config) (*Server, error) {
	if cfg.CacheDir == "" {
		return nil, errors.New("CacheDir is required")
	}
	logDir := filepath.Join(cfg.CacheDir, "tasks")
	migrateTaskLogs(cfg.CacheDir, logDir)

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	ctx, startTask := trace.NewTask(ctx, "server.startup")
	defer startTask.End()

	// container.New is instant; run it serially to simplify.
	mdClient, err := container.New(cfg.TailscaleAPIKey, cfg.GitHubToken)
	if err != nil {
		return nil, fmt.Errorf("init container library: %w", err)
	}
	mdClient.DigestCacheTTL = warmupInterval

	// Phase 1: Parallel I/O — repos discovery, logs loading, and container listing.
	type reposResult struct {
		paths []string
		err   error
	}
	type logsResult struct {
		logs []*task.LoadedTask
		err  error
	}
	type containersResult struct {
		containers []*md.Container
		err        error
	}

	repoCh := make(chan reposResult, 1)
	logCh := make(chan logsResult, 1)
	contCh := make(chan containersResult, 1)

	go func() {
		defer trace.StartRegion(ctx, "discover-repos").End()
		paths, err := gitutil.DiscoverRepos(rootDir, repoDiscoveryDepth)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		defer trace.StartRegion(ctx, "load-logs").End()
		logs, err := task.LoadLogs(logDir)
		logCh <- logsResult{logs, err}
	}()
	go func() {
		defer trace.StartRegion(ctx, "list-containers").End()
		containers, err := mdClient.List(ctx)
		contCh <- containersResult{containers, err}
	}()

	repoRes := <-repoCh
	logRes := <-logCh
	contRes := <-contCh

	// Check for errors.
	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
	}
	// Load persistent settings (generates sessionSecret on first run).
	settings, err := loadSettings(filepath.Join(cfg.ConfigDir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	// Initialize host checking and external URL state.
	var hostState *auth.HostState
	isAuto := strings.EqualFold(cfg.ExternalURL, "auto")
	if isAuto {
		hostState = &auth.HostState{}
	} else if cfg.ExternalURL != "" {
		hostState = auth.NewHostState(cfg.ExternalURL)
	}

	slog.Info("github", "pat", auth.MaskedToken(cfg.GitHubToken), "oauth", auth.MaskedToken(cfg.GitHubOAuthClientID))
	slog.Info("gitlab", "pat", auth.MaskedToken(cfg.GitLabToken), "oauth", auth.MaskedToken(cfg.GitLabOAuthClientID))

	// Initialize auth store and OAuth providers when auth is configured.
	var authStore *auth.Store
	var sessionSecret []byte
	var githubOAuth *auth.ProviderConfig
	var gitlabOAuth *auth.ProviderConfig
	oauthConfigured := cfg.GitHubOAuthClientID != "" || cfg.GitLabOAuthClientID != ""
	if cfg.ExternalURL != "" && (oauthConfigured || !isAuto) {
		secret, err := hexDecode(settings.SessionSecret)
		if err != nil {
			return nil, fmt.Errorf("decode session secret: %w", err)
		}
		sessionSecret = secret
		store, err := auth.Open(filepath.Join(cfg.ConfigDir, "users.json"))
		if err != nil {
			return nil, fmt.Errorf("open users store: %w", err)
		}
		authStore = store
		if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
			c := auth.GitHubConfig(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, hostState)
			githubOAuth = &c
		}
		if cfg.GitLabOAuthClientID != "" && cfg.GitLabOAuthClientSecret != "" {
			c := auth.GitLabConfig(cfg.GitLabOAuthClientID, cfg.GitLabOAuthClientSecret, cfg.GitLabURL, hostState)
			gitlabOAuth = &c
		}
	}

	githubAllowedUsers := parseAllowedUsers(cfg.GitHubOAuthAllowedUsers)
	gitlabAllowedUsers := parseAllowedUsers(cfg.GitLabOAuthAllowedUsers)

	prefsPath := filepath.Join(cfg.ConfigDir, "preferences.json")
	prefsStore, err := preferences.Open(prefsPath)
	if err != nil {
		return nil, fmt.Errorf("open preferences: %w", err)
	}

	backend := container.NewBackend(mdClient)
	backend.HarnessEnv = cfg.HarnessEnv

	cachePath := filepath.Join(cfg.CacheDir, "ci_results.json")
	cache, err := forgecache.Open(cachePath)
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "path", cachePath, "err", err)
		cache, _ = forgecache.Open("")
	}

	var voiceBridge *voicertc.Bridge
	if cfg.WebRTCPort >= 0 && cfg.GeminiAPIKey != "" {
		voiceBridge, err = voicertc.NewBridge(ctx, cfg.GeminiAPIKey, cfg.WebRTCPort)
		if err != nil {
			return nil, fmt.Errorf("voice bridge: %w", err)
		}
	} else if cfg.WebRTCPort >= 0 {
		slog.Info("voice bridge disabled: GEMINI_API_KEY not set")
	}

	s := &Server{
		ctx:                ctx,
		absRoot:            absRoot,
		runners:            make(map[string]*task.Runner, len(repoRes.paths)),
		mdClient:           mdClient,
		logDir:             logDir,
		cacheDir:           cfg.CacheDir,
		prefs:              prefsStore,
		authStore:          authStore,
		sessionSecret:      sessionSecret,
		githubOAuth:        githubOAuth,
		gitlabOAuth:        gitlabOAuth,
		githubAllowedUsers: githubAllowedUsers,
		gitlabAllowedUsers: gitlabAllowedUsers,
		hostState:          hostState,
		usageFetchers:      detectProviders(ctx, cfg.HarnessEnv),
		pprof:              cfg.Pprof,
		geminiAPIKey:       cfg.GeminiAPIKey,
		voiceBridge:        voiceBridge,
		forge:              newForgeManager(cfg.GitHubToken, cfg.GitLabToken, nil),
		ciCache:            cache,
		backend:            backend,
		tasks:              make(map[string]*taskEntry),
		repoCIStatus:       make(map[string]ci.RepoCIState),
		changed:            make(chan struct{}),
	}
	s.githubWebhookSecret = cfg.GitHubWebhookSecret
	s.gitlabWebhookSecret = cfg.GitLabWebhookSecret

	if cfg.GitHubAppID != 0 && len(cfg.GitHubAppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPEM, s.forge.githubAppThrottle)
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		s.forge.githubApp = app
		if cfg.GitHubAppAllowedOwners != "" {
			s.githubAppAllowedOwners = parseAllowedUsers(cfg.GitHubAppAllowedOwners)
		}
	}

	// Determine LLM provider: use configured value or auto-detect
	llmProvider := cfg.LLMProvider
	if llmProvider == "" {
		llmProvider = autoDetectLLMProvider(ctx, cfg.GeminiAPIKey)
		if llmProvider != "" {
			slog.Info("auto-detected LLM provider", "prov", llmProvider)
		}
	}

	if llmProvider != "" {
		if c, ok := providers.All[llmProvider]; !ok || c.Factory == nil {
			slog.Warn("unknown LLM provider for title generation", "prov", llmProvider)
		} else {
			var opts []genai.ProviderOption
			if cfg.LLMModel != "" {
				opts = append(opts, genai.ProviderOptionModel(cfg.LLMModel))
			} else {
				opts = append(opts, genai.ModelCheap)
			}
			// Pass API key if configured for the provider.
			if llmProvider == "gemini" && cfg.GeminiAPIKey != "" {
				opts = append(opts, genai.ProviderOptionAPIKey(cfg.GeminiAPIKey))
			}
			if p, err := c.Factory(ctx, opts...); err != nil {
				slog.Warn("LLM provider init failed", "prov", llmProvider, "err", err)
			} else {
				slog.Info("title", "prov", p.Name(), "mdl", p.ModelID())
				s.provider = p
				backend.Provider = p
			}
		}
	}

	// Phase 2: Runner init (parallel per-repo).
	type repoResult struct {
		info   repoInfo
		runner *task.Runner
	}
	results := make([]repoResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			defer trace.StartRegion(ctx, "repo-runner-init").End()
			rel, err := filepath.Rel(absRoot, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			remoteName, err := gitutil.DefaultRemote(ctx, abs)
			if err != nil {
				slog.Warn("skipping repo, cannot determine default remote", "path", abs, "err", err)
				return
			}
			branch, err := gitutil.DefaultBranch(ctx, abs, remoteName)
			if err != nil {
				slog.Warn("skipping repo, cannot determine default branch", "path", abs, "err", err)
				return
			}
			remote := gitutil.RemoteOriginURL(ctx, abs)
			runner := &task.Runner{
				BaseBranch: branch,
				Dir:        abs,
				LogDir:     logDir,
				CacheDir:   cfg.CacheDir,
				HarnessEnv: cfg.HarnessEnv,
				Container:  backend,
			}
			if err := runner.Init(ctx); err != nil {
				slog.Warn("runner init failed", "path", abs, "err", err)
			}
			var forgeKind forge.Kind
			var forgeOwner, forgeRepo string
			if rawURL, err := forge.RemoteURL(ctx, abs); err == nil {
				forgeKind, forgeOwner, forgeRepo, _ = forge.ParseRemoteURL(rawURL)
			}
			results[i] = repoResult{
				info: repoInfo{
					RelPath: rel, AbsPath: abs, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote,
					ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo,
				},
				runner: runner,
			}
			slog.Debug("discovered repo", "path", rel, "br", branch)
		})
	}
	wg.Wait()
	for i := range results {
		if results[i].runner == nil {
			continue
		}
		s.repos = append(s.repos, results[i].info)
		s.runners[results[i].info.RelPath] = results[i].runner
	}

	// Wire the bot with the server as its client.
	// Eventually we may want to use a clearer observer pattern.
	s.Bot = bot.New(ctx, s)

	// Always register a no-repo runner (keyed by "") for tasks that don't
	// need a git repository.
	noRepoRunner := &task.Runner{LogDir: logDir, CacheDir: cfg.CacheDir, HarnessEnv: cfg.HarnessEnv, Container: backend}
	_ = noRepoRunner.Init(ctx) // populates Backends; no-op for no-repo (no branches to scan)
	s.runners[""] = noRepoRunner

	// Phase 3: Load purged tasks from pre-loaded logs.
	phase3 := trace.StartRegion(ctx, "load-purged-tasks")
	if logRes.err != nil {
		slog.Warn("load logs failed", "err", logRes.err)
	} else {
		if err := s.loadPurgedTasksFrom(logRes.logs); err != nil {
			phase3.End()
			return nil, fmt.Errorf("load purged tasks: %w", err)
		}
	}
	phase3.End()

	// Phase 4: Adopt containers (using pre-fetched list).
	phase4 := trace.StartRegion(ctx, "adopt-containers")
	if contRes.err != nil {
		slog.Warn("list containers failed, skipping adoption", "err", contRes.err)
	} else {
		if err := s.adoptContainers(ctx, contRes.containers, logRes.logs); err != nil {
			phase4.End()
			return nil, fmt.Errorf("adopt containers: %w", err)
		}
	}
	phase4.End()

	// Resume bot comment watchers for adopted tasks with pending forge issues.
	{
		region := trace.StartRegion(ctx, "bot-resume-comments")
		s.Bot.ResumePendingComments()
		region.End()
	}

	s.ipgeoChecker, err = ipgeo.NewChecker(ctx, cfg.IPGeoAllowlist, cfg.IPGeoDB)
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeoDB != "" {
		slog.Info("ipgeo", "path", cfg.IPGeoDB, "list", cfg.IPGeoAllowlist)
	}

	s.watchContainerEvents(ctx)
	go func() {
		_, tk := trace.NewTask(ctx, "warmup-images")
		defer tk.End()
		trace.Log(ctx, "startup", "warmup-images: begin")
		s.warmupImages()
	}()
	go func() {
		_, tk := trace.NewTask(ctx, "refresh-harness-models")
		defer tk.End()
		trace.Log(ctx, "startup", "refresh-harness-models: begin")
		s.refreshHarnessModels() //nolint:contextcheck // server-lifetime goroutine, uses s.ctx internally
	}()
	go s.pollStats(s.ctx) //nolint:contextcheck // server-lifetime context is intentional
	go s.watchNewRepos()
	{
		region := trace.StartRegion(ctx, "ci-new-service")
		s.ciService = ci.NewService(s.ciCache, s.provider, s)
		region.End()
	}
	return s, nil
}

// migrateTaskLogs moves *.jsonl files from cacheDir into the tasks
// subdirectory. This is a one-time migration for installations that stored
// task logs directly in CacheDir.
// TODO: Remove after 2026-05-01.
func migrateTaskLogs(cacheDir, tasksDir string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	var jsonlFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			jsonlFiles = append(jsonlFiles, e)
		}
	}
	if len(jsonlFiles) == 0 {
		return
	}
	if err := os.MkdirAll(tasksDir, 0o750); err != nil {
		slog.Warn("migrate: cannot create tasks dir", "path", tasksDir, "err", err)
		return
	}
	for _, e := range jsonlFiles {
		src := filepath.Join(cacheDir, e.Name())
		dst := filepath.Join(tasksDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			slog.Warn("migrate: cannot move log", "file", e.Name(), "err", err)
		}
	}
	slog.Info("migrated task logs", "n", len(jsonlFiles), "dst", tasksDir)
}

// loadPurgedTasks loads the last 5 purged tasks per repository from JSONL logs on disk.
// Exported for testing; New() uses the parallelized variant.
func (s *Server) loadPurgedTasks() error {
	all, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	return s.loadPurgedTasksFrom(all)
}

// setParser sets the parse function on a LoadedTask from the first runner
// that has a backend for the task's harness.
func (s *Server) setParser(lt *task.LoadedTask) {
	for _, r := range s.runners {
		if b := r.Backends[lt.Harness]; b != nil {
			lt.SetParser(b.NewParser())
			return
		}
	}
}

// loadTaskMessagesOnDemand triggers lazy message loading for purged tasks
// the first time the full conversation is needed (e.g. when the user opens
// task detail and subscribes to SSE events). Must be called before
// Subscribe to populate the history snapshot.
func (s *Server) loadTaskMessagesOnDemand(entry *taskEntry) {
	if entry.loadedTask == nil {
		return
	}
	entry.loadedTaskOnce.Do(func() {
		if err := entry.loadedTask.LoadMessages(); err != nil {
			slog.Warn("lazy load messages failed", "task", entry.task.ID, "err", err)
			return
		}
		entry.task.RestoreMessages(entry.loadedTask.Msgs)
	})
}

// loadPurgedTasksFrom populates s.tasks from pre-loaded log data.
//
// It keeps tasks updated within the last few days and limits the result to the N most recent per repository.
// Tasks without a caic_result trailer get a synthetic result; their state is inferred from messages and
// finalised in the setup loop. adoptContainers removes all stale entries for any branch that has a live
// container, so no-trailer tasks never duplicate adopted ones.
func (s *Server) loadPurgedTasksFrom(all []*task.LoadedTask) error {
	// Include all tasks updated within the last few days, with or without a
	// caic_result trailer. Trailer-less tasks (interrupted or still-running)
	// are deduplicated by adoptContainers which sweeps all stale entries for
	// each branch it adopts.
	const oldest = 14 * 24 * time.Hour
	const maxPurgedPerRepo = 5
	var purged []*task.LoadedTask
	now := time.Now().UTC()
	for _, lt := range all {
		if now.Sub(lt.LastStateUpdateAt) > oldest {
			continue
		}
		if lt.Result == nil {
			lt.Result = &task.Result{State: task.StateFailed}
		}
		purged = append(purged, lt)
	}
	// Sort by last state update descending so the max-per-repo limit keeps the
	// most recently active tasks, not just the most recently started ones.
	slices.SortFunc(purged, func(a, b *task.LoadedTask) int {
		return b.LastStateUpdateAt.Compare(a.LastStateUpdateAt)
	})
	perRepo := make(map[string]int)
	kept := purged[:0]
	for _, lt := range purged {
		key := ""
		if p := lt.Primary(); p != nil {
			key = p.Name
		}
		if perRepo[key] < maxPurgedPerRepo {
			perRepo[key]++
			kept = append(kept, lt)
		}
	}
	purged = kept
	if len(purged) == 0 {
		slog.Info("no purged tasks to load", "candidates", len(all))
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lt := range purged {
		taskID := ksid.NewID()
		// The original ID is embedded in the log filename as the prefix before the
		// first '-'. Real server IDs are 10–12 chars (current-era timestamps in
		// base32). Reject short strings (e.g. "a" from test filenames) that parse
		// to implausibly small values.
		if len(lt.TaskID) >= 9 {
			if parsed, parseErr := ksid.Parse(lt.TaskID); parseErr == nil && parsed != 0 {
				taskID = parsed
			}
		}
		t := &task.Task{
			ID:            taskID,
			InitialPrompt: agent.Prompt{Text: lt.Prompt},
			Model:         lt.Model,
			Repos:         lt.Repos, // GitRoot is empty for purged tasks
			Harness:       lt.Harness,
			StartedAt:     lt.StartedAt,
			Tailscale:     lt.Tailscale,
			USB:           lt.USB,
			Display:       lt.Display,
		}
		t.SetStateAt(lt.State, lt.LastStateUpdateAt)
		if lt.AgentVersion != "" {
			t.SetAgentVersion(lt.AgentVersion)
		}
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		// Purged tasks get all metadata from the caic_result trailer and
		// the header-only tail scan (64 KiB). Full message parse is never
		// needed. adoptContainers corrects the state for trailer-less tasks
		// that are still running.
		if lt.State == task.StateRunning {
			t.SetState(task.StateFailed)
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
		// Stats backfill from messages is handled by loadLogHeader's tail
		// scan when the trailer has zero cost (see case "result").
		// Set up the parser so messages can be lazily loaded on demand.
		s.setParser(lt)
		done := make(chan struct{})
		close(done)
		entry := &taskEntry{task: t, result: lt.Result, done: done, loadedTask: lt}
		s.tasks[t.ID.String()] = entry
	}
	s.taskChanged()
	slog.Info("loaded purged tasks from logs", "n", len(purged))
	return nil
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI.
//
// Flow:
//  1. Map branches from purged tasks to their IDs so live containers
//     can replace stale entries.
//  2. For each container matching a caic repo, call adoptOne concurrently.
//
// containers and allLogs are pre-loaded to avoid redundant I/O. If containers
// is nil (due to a container client error), adoption is skipped.
func (s *Server) adoptContainers(ctx context.Context, containers []*md.Container, allLogs []*task.LoadedTask) error {
	if containers == nil {
		return nil
	}

	// Map repo+branch loaded from purged task logs to their ID in
	// s.tasks so we can replace stale entries with live containers.
	// The key is "repo\x00branch" because different repos can share a
	// branch name.
	s.mu.Lock()
	// Map repo+branch → all stale task IDs so adoptOne can remove every
	// matching entry (there may be multiple log files per branch when a
	// branch was reused or when trailer-less tasks were loaded alongside
	// properly-purged ones with the same branch).
	branchIDs := make(map[string][]string, len(s.tasks))
	for id, e := range s.tasks {
		if p := e.task.Primary(); p != nil && p.Branch != "" {
			key := p.Name + "\x00" + p.Branch
			branchIDs[key] = append(branchIDs[key], id)
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	claimed := make(map[string]struct{}, len(containers))

	for i := range s.repos {
		ri := &s.repos[i]
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, c := range containers {
			branch, ok := container.BranchFromContainer(c.Name, repoName)
			if !ok {
				continue
			}
			claimed[c.Name] = struct{}{}
			wg.Go(func() {
				if err := s.adoptOne(ctx, *ri, runner, c, branch, branchIDs, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()

	// Adopt no-repo containers. md names them "md-agent-<hex>" when started
	// with no repos (md.Client.Container with zero Repo arguments).
	if noRepoRunner := s.runners[""]; noRepoRunner != nil {
		for _, c := range containers {
			if _, ok := claimed[c.Name]; ok || !strings.HasPrefix(c.Name, "md-agent-") {
				continue
			}
			wg.Go(func() {
				if err := s.adoptOne(ctx, repoInfo{}, noRepoRunner, c, "", branchIDs, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}

	return errors.Join(errs...)
}

// adoptOne investigates a single container and registers it as a task.
//
// It verifies the container has a "caic" label (proving caic started it),
// restores messages from either the relay output or JSONL logs, checks
// whether the relay is alive, and registers the task. If the relay is
// alive, it spawns a background goroutine to reattach. allLogs is the
// pre-loaded set of JSONL log files (shared across all adoptOne calls).
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch string, branchIDs map[string][]string, allLogs []*task.LoadedTask) error { //nolint:gocritic // repoInfo size increase from GitHub fields; refactor not worth it
	ctx, adoptTask := trace.NewTask(ctx, "adopt-container")
	defer adoptTask.End()
	trace.Logf(ctx, "container", "%s repo=%s branch=%s", c.Name, ri.RelPath, branch)

	// Only adopt containers that caic started. The caic label is set at
	// container creation and is the authoritative proof of ownership.
	caicLabelReg := trace.StartRegion(ctx, "caic-label")
	trace.Logf(ctx, "adopt", "%s: label-caic", c.Name)
	labelVal, err := container.LabelValue(ctx, c.Name, "caic")
	caicLabelReg.End()
	if err != nil {
		return fmt.Errorf("label check for %s: %w", c.Name, err)
	}
	if labelVal == "" {
		slog.Info("container", "msg", "skipping non-caic", "repo", ri.RelPath, "ctr", c.Name, "br", branch)
		return nil
	}
	taskID, err := ksid.Parse(labelVal)
	if err != nil {
		return fmt.Errorf("parse caic label %q on %s: %w", labelVal, c.Name, err)
	}

	// Exited containers are adopted as stopped tasks. The user can
	// explicitly revive them via the UI or API when ready.
	isExited := c.State == "exited"
	if isExited {
		slog.Info("container", "msg", "adopting exited container as stopped", "ctr", c.Name, "br", branch)
	}

	// Find the log file for this task. For repo-based tasks, match by repo+branch
	// (most recent first) since different repos can share branch names. For no-repo
	// tasks (branch==""), match by task ID parsed from the filename, which is the
	// only reliable disambiguator when multiple no-repo tasks share the same empty
	// repo+branch values.
	taskIDStr := taskID.String()
	var lt *task.LoadedTask
	for _, log := range slices.Backward(allLogs) {
		if branch == "" && ri.RelPath == "" {
			if log.TaskID == taskIDStr {
				lt = log
				break
			}
		} else {
			lp := log.Primary()
			if lp != nil && lp.Branch == branch && lp.Name == ri.RelPath {
				lt = log
				break
			}
		}
	}

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Read the harness from the container label (authoritative), falling
	// back to the log file, then to Claude as the default.
	trace.Logf(ctx, "adopt", "%s: label-harness", c.Name)
	harnessLabel, _ := container.LabelValue(ctx, c.Name, "harness")
	harnessName := agent.Harness(harnessLabel)
	if harnessName == "" && lt != nil {
		harnessName = lt.Harness
	}
	if harnessName == "" {
		harnessName = agent.Claude
	}

	// Check whether the relay daemon is alive in this container.
	// Skip for exited containers — can't exec into them.
	var relayAlive bool
	var relayMsgs []agent.Message
	var relaySize int64
	var relayDiag string
	if !isExited {
		var relayErr error
		relayStatusReg := trace.StartRegion(ctx, "relay-status")
		trace.Logf(ctx, "adopt", "%s: relay-status", c.Name)
		relayAlive, relayDiag, relayErr = agent.RelayStatus(ctx, c.Name)
		relayStatusReg.End()
		if relayErr != nil {
			slog.Warn("relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr, "diag", relayDiag)
		}
		if relayAlive {
			// Relay is alive — read authoritative output from container.
			relayReadReg := trace.StartRegion(ctx, "relay-read")
			trace.Logf(ctx, "adopt", "%s: relay-read", c.Name)
			relayMsgs, relaySize, relayErr = runner.ReadRelayOutput(ctx, c.Name, harnessName)
			relayReadReg.End()
			if relayErr != nil {
				slog.Warn("relay", "msg", "read output failed", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr)
				relayAlive = false
			}
		}
	}

	if lt != nil && lt.Prompt != "" {
		prompt = lt.Prompt
		startedAt = lt.StartedAt
		stateUpdatedAt = lt.LastStateUpdateAt
	}

	if stateUpdatedAt.IsZero() {
		stateUpdatedAt = time.Now().UTC()
	}
	var adoptRepos []task.RepoMount
	if ri.RelPath != "" {
		// Primary mount from repoInfo; extra mounts from log.
		// Restore BaseBranch from the log so that tasks created with
		// a non-default base (e.g. "develop" vs. repo default "main")
		// survive server restarts.
		primaryBaseBranch := ""
		if lt != nil && lt.Primary() != nil {
			primaryBaseBranch = lt.Primary().BaseBranch
		}
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, BaseBranch: primaryBaseBranch, GitRoot: ri.AbsPath, Branch: branch}}
		if lt != nil {
			for _, lm := range lt.Repos[1:] {
				gitRoot := ""
				if er, ok := s.runners[lm.Name]; ok {
					gitRoot = er.Dir
				}
				adoptRepos = append(adoptRepos, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot})
			}
		}
	}
	var forgeIssue int
	if lt != nil {
		forgeIssue = lt.ForgeIssue
	}
	t := &task.Task{
		ID:            taskID,
		InitialPrompt: agent.Prompt{Text: prompt},
		Repos:         adoptRepos,
		Harness:       harnessName,
		Container:     c.Name,
		StartedAt:     startedAt,
		Tailscale:     c.Tailscale,
		TailscaleFQDN: c.TailscaleFQDN(ctx),
		USB:           c.USB,
		Display:       c.Display,
		VNCPort:       int(c.VNCPort),
		Provider:      s.provider,
		ForgeIssue:    forgeIssue,
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	// Set an immediate fallback title; GenerateTitle is fired async below
	// after messages are restored so the LLM sees the full conversation.
	if lt != nil && lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
	}
	switch {
	case lt != nil && lt.ForgePR > 0:
		// Restore PR created during a previous session (persisted in log).
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
	case forgeIssue > 0 && ri.ForgeOwner != "":
		// Ensure forge owner/repo are set so the bot can resolve a commenter.
		t.SetPR(ri.ForgeOwner, ri.ForgeRepo, 0)
	}

	// Restore messages from relay or logs.
	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss. It contains both
		// Claude Code stdout and user inputs (logged by the relay).
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Debug("relay", "msg", "restored from", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(relayMsgs))
	} else if lt != nil {
		s.setParser(lt)
		loadMsgsReg := trace.StartRegion(ctx, "load-messages")
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
		loadMsgsReg.End()
		if len(lt.Msgs) > 0 {
			t.RestoreMessages(lt.Msgs)
			slog.Warn("relay", "msg", "restored from log", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(lt.Msgs))
		}
	}
	// RestoreMessages may infer a new state (e.g. waiting) from trailing
	// messages, but setState stamps time.Now(). Re-apply the original
	// timestamp so the UI timer reflects when the agent actually stopped
	// producing output, not when the server restarted.
	t.SetStateAt(t.GetState(), stateUpdatedAt)

	// The header-only tail scan may miss caic_pr when the record is beyond
	// the 64 KiB window. If the PR is still unset, do a full parse of the
	// log to recover it. This covers both the relay-alive path (where
	// LoadMessages was skipped) and the log-restore path.
	if lt != nil && t.GetPR() == 0 {
		if lt.ForgePR == 0 {
			// Full parse not yet done; trigger it for PR metadata only.
			_ = lt.LoadMessages()
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
	}

	// If the task is still running after message restoration (agent is
	// mid-turn), record now as the turn start. This is the best available
	// approximation on adoption; the real turn start predates the restart.
	if !isExited {
		t.SetTurnStartedAt(time.Now().UTC())
	}

	// Exited containers are always stopped — user must revive explicitly.
	if isExited {
		t.SetState(task.StateStopped)
	} else if !relayAlive {
		// Relay is dead but container is running. Read relay log for
		// diagnostics, then mark waiting so the user can restart or
		// we can auto-reconnect via --resume.
		relayLog := agent.ReadRelayLog(ctx, c.Name, 4096)
		if relayLog != "" {
			slog.Warn("relay", "msg", "log from dead relay", "ctr", c.Name, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		trace.Logf(ctx, "adopt", "%s: relay-dead", c.Name)
		if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			slog.Warn("relay", "msg", "dead, marking waiting",
				"repo", ri.RelPath, "br", branch, "ctr", c.Name,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entryRegistered := false
	entry := &taskEntry{task: t, done: make(chan struct{})}

	// Register entry and start CI monitoring if a PR was restored from the log.
	if t.GetPR() > 0 && ri.ForgeOwner != "" && ri.ForgeKind != "" {
		// The adoption context has no authenticated user. Try the general
		// lookup first (PAT / GitHub App), then fall back to a stored
		// OAuth token from the auth store (most recently seen user for
		// this forge provider).
		f := s.forge.forgeForInfo(ctx, &ri)
		if f == nil && s.authStore != nil {
			if u, ok := s.authStore.FindByProvider(ri.ForgeKind); ok {
				f = s.forge.forgeFor(auth.NewContext(ctx, &u), ri.ForgeKind)
			}
		}
		slog.Info("adopt: CI monitoring", "task", t.ID, "pr", t.GetPR(), "forgeKind", ri.ForgeKind, "forgeOwner", ri.ForgeOwner, "hasForge", f != nil)
		if f != nil {
			s.mu.Lock()
			if ri.RelPath != "" || branch != "" {
				for _, oldID := range branchIDs[ri.RelPath+"\x00"+branch] {
					delete(s.tasks, oldID)
				}
			}
			s.tasks[t.ID.String()] = entry
			s.taskChanged()
			s.mu.Unlock()
			entryRegistered = true
			// Get the PR head SHA for CI monitoring.
			pr := t.Snapshot().ForgePR
			if pr > 0 {
				sha, err := f.GetDefaultBranchSHA(ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
				if err != nil {
					slog.Warn("adopt: GetDefaultBranchSHA failed", "task", t.ID, "branch", branch, "err", err)
				} else {
					slog.Info("adopt: starting monitorCI", "task", t.ID, "branch", branch, "sha", sha)
					s.mu.Lock()
					entry.monitorBranch = branch
					s.mu.Unlock()
					go s.ciService.MonitorCI(s.ctx, entry, f, ri.ForgeOwner, ri.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive the request
				}
			}
		}
	}

	if !entryRegistered {
		s.mu.Lock()
		if ri.RelPath != "" || branch != "" {
			for _, oldID := range branchIDs[ri.RelPath+"\x00"+branch] {
				delete(s.tasks, oldID)
			}
		}
		s.tasks[t.ID.String()] = entry
		s.taskChanged()
		s.mu.Unlock()
	}

	// External PR lookup: deferred so the forge API call doesn't block startup.
	// Applies when the log has no PR (log-based PRs are set synchronously above)
	// and this is not a bot-driven issue task.
	if forgeIssue == 0 && t.GetPR() == 0 && ri.ForgeOwner != "" && branch != "" && ri.ForgeKind != "" {
		go s.lookupExternalPR(&ri, branch, t, entry) //nolint:contextcheck // server-lifetime context is intentional; must outlive adoption
	}

	slog.Info("container", "msg", "adopted",
		"repo", ri.RelPath, "ctr", c.Name, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed since the log was
	// written (relay captured ResultMessages beyond what the log has).
	// Count results in the restored messages; if the relay has more than the
	// log, a turn happened while the server was down and the title is stale.
	if needsTitleRegen(t, lt) {
		go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive adoption
	}

	// Auto-reconnect in background: attach to the live relay.
	// If the relay is dead, the session is lost — no resume attempt.
	// Skip reconnect for stopped tasks — container is not running.
	if t.GetState() != task.StateStopped && relayAlive {
		slog.Debug("container", "msg", "auto-reconnect starting", "repo", ri.RelPath, "br", branch, "ctr", c.Name)
		go func() {
			tlog := slog.With("repo", ri.RelPath, "br", branch, "ctr", t.Container)
			h, err := runner.Reconnect(ctx, t, true)
			if err != nil {
				tlog.Warn("auto-reconnect failed", "err", err)
				s.notifyTaskChange()
				return
			}
			h, err = runner.EnsureSession(ctx, t, h, tlog)
			if err != nil {
				tlog.Warn("ensure session failed", "err", err)
				t.SetState(task.StateWaiting)
				s.notifyTaskChange()
				return
			}
			tlog.Debug("auto-reconnect succeeded")
			// Repopulate VNC port from Docker (not in container labels).
			t.VNCPort = runner.Container.VNCPort(ctx, t.Container)
			// Compute host-side diff stat after reconnect. Reconnect
			// replays relay messages which may include stale
			// DiffStatMessages (old relay code diffs against HEAD, not
			// base); the host-side diff captures the full branch diff.
			var adoptPrimaryBranch string
			if p := t.Primary(); p != nil {
				adoptPrimaryBranch = p.Branch
			}
			if ds := runner.BranchDiffStat(ctx, adoptPrimaryBranch, t.ExtraMDRepos()); len(ds) > 0 {
				t.SetLiveDiffStat(ds)
			}
			s.notifyTaskChange()
			s.watchSession(entry, runner, h)
		}()
	} else if !relayAlive && t.GetState() != task.StateStopped {
		slog.Error("relay dead, stopping container",
			"repo", ri.RelPath, "br", branch, "ctr", c.Name,
			"state", t.GetState())
		t.SetState(task.StateStopping)
		if err := runner.Container.Stop(ctx, c.Name); err != nil {
			slog.Error("stop failed", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", err)
		}
		t.SetState(task.StateStopped)
	}
	return nil
}

// lookupExternalPR queries the forge for a PR matching branch and, if found,
// updates t, notifies clients, and starts CI monitoring. Runs in a goroutine
// so the forge API call does not block server startup.
func (s *Server) lookupExternalPR(ri *repoInfo, branch string, t *task.Task, entry *taskEntry) {
	f := s.forge.forgeForInfo(s.ctx, ri)
	if f == nil && s.authStore != nil {
		if u, ok := s.authStore.FindByProvider(ri.ForgeKind); ok {
			f = s.forge.forgeFor(auth.NewContext(s.ctx, &u), ri.ForgeKind)
		}
	}
	if f == nil {
		return
	}
	pr, err := f.FindPRByBranch(s.ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
	if err != nil || pr.Number == 0 {
		return
	}
	slog.Info("adopt: found external PR", "repo", ri.RelPath, "br", branch, "pr", pr.Number)
	t.SetPR(ri.ForgeOwner, ri.ForgeRepo, pr.Number)
	s.notifyTaskChange()
	sha, err := f.GetDefaultBranchSHA(s.ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
	if err != nil {
		slog.Warn("adopt: GetDefaultBranchSHA failed", "task", t.ID, "branch", branch, "err", err)
		return
	}
	slog.Info("adopt: starting monitorCI", "task", t.ID, "branch", branch, "sha", sha)
	s.mu.Lock()
	entry.monitorBranch = branch
	s.mu.Unlock()
	s.ciService.MonitorCI(s.ctx, entry, f, ri.ForgeOwner, ri.ForgeRepo, sha)
}

// watchContainerEvents starts a single goroutine that listens for Docker
// container die events and triggers cleanup for the corresponding task.
func (s *Server) watchContainerEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := container.WatchEvents(ctx, "caic")
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("docker events failed, retrying in 5s", "err", err)
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
			for ev := range ch {
				s.handleContainerDeath(ev.Name)
			}
			// Stream ended. Reconnect unless context cancelled.
			if ctx.Err() != nil {
				return
			}
			slog.Warn("docker events stream ended, reconnecting in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// warmupInterval controls how often warmupImages re-checks for new base image
// versions. It also sets DigestCacheTTL so that container starts between
// warmup cycles reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

// warmupImages periodically calls md.Client.Warmup for the default base image
// and any custom images configured in user preferences. This ensures the image
// is pulled and the md-user layer is built before a task needs it.
func (s *Server) warmupImages() {
	// Run immediately on startup, then every warmupInterval.
	ticker := time.NewTicker(warmupInterval)
	defer ticker.Stop()
	for {
		images := []string{md.DefaultBaseImage + ":latest"}
		for _, img := range s.prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			w := &container.SlogWriter{Phase: "warmup"}
			built, err := s.mdClient.Warmup(s.ctx, w, w, &md.WarmupOpts{
				BaseImage: img,
				Quiet:     true,
			})
			if err != nil {
				slog.Warn("warmup", "image", img, "err", err)
			} else if built {
				slog.Info("warmup", "image", img, "built", true)
			}
		}
		select {
		case <-ticker.C:
		case <-s.ctx.Done():
			return
		}
	}
}

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary container. Runs once at startup.
func (s *Server) refreshHarnessModels() {
	cache := agent.OpenHarnessCache(filepath.Join(s.cacheDir, "harnesses.json"))

	type fetchFunc func(ctx context.Context, container string, env []string) ([]string, error)
	harnesses := []struct {
		h     agent.Harness
		fetch fetchFunc
	}{
		{agent.Pi, pi.FetchModels},
		{agent.OpenCode, opencode.FetchModels},
	}
	for _, entry := range harnesses {
		if _, fresh := cache.Models(entry.h); fresh {
			continue
		}
		s.refreshOneHarness(cache, entry.h, entry.fetch)
	}
}

// refreshOneHarness launches a temporary container, fetches models, and
// updates the cache and all runner backends.
func (s *Server) refreshOneHarness(cache *agent.HarnessCache, h agent.Harness, fetch func(ctx context.Context, container string, env []string) ([]string, error)) {
	slog.Info("model cache stale, fetching from temporary container", "harness", h)
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	w := &container.SlogWriter{Phase: "model-refresh"}
	name, err := s.backend.Launch(ctx, nil, []string{"model-refresh"}, &task.StartOptions{
		Harness:   h,
		LogWriter: w,
	})
	if err != nil {
		slog.Warn("model refresh: launch failed", "harness", h, "err", err)
		return
	}
	defer func() {
		_ = s.backend.Purge(context.WithoutCancel(ctx), name, nil)
	}()
	if _, err := s.backend.Connect(ctx, name, nil, &task.StartOptions{Harness: h, LogWriter: w}); err != nil {
		slog.Warn("model refresh: connect failed", "harness", h, "err", err)
		return
	}
	models, err := fetch(ctx, name, s.backend.HarnessEnv[string(h)])
	if err != nil {
		slog.Warn("model refresh: fetch failed", "harness", h, "err", err)
		return
	}
	cache.SetModels(h, models)
	slog.Info("model cache refreshed", "harness", h, "count", len(models))

	for _, r := range s.runners {
		if b, ok := r.Backends[h]; ok {
			b.SetModels(models)
		}
	}
}

// handleContainerDeath looks up a task by container name and archives it.
// The container is not destroyed — it transitions to StateStopped so it
// can be revived on the next server restart (e.g. after a Docker or
// machine restart).
func (s *Server) handleContainerDeath(containerName string) {
	s.mu.Lock()
	var found *taskEntry
	for _, e := range s.tasks {
		if e.task.Container != containerName {
			continue
		}
		found = e
		break
	}
	s.mu.Unlock()
	if found == nil {
		return
	}
	t := found.task
	state := t.GetState()
	// Only archive active tasks. Already-terminal tasks should not be touched.
	if state == task.StatePurged || state == task.StateFailed || state == task.StateStopped || state == task.StateStopping {
		return
	}
	deathBranch := ""
	if p := t.Primary(); p != nil {
		deathBranch = p.Branch
	}
	slog.Info("container", "msg", "died, archiving as stopped", "ctr", containerName, "task", t.ID, "br", deathBranch, "prev_state", state)
	// Detach any active session (SSH is dead).
	t.DetachSession()
	t.SetState(task.StateStopped)
	s.notifyTaskChange()
}

// watchNewRepos polls absRoot and its subdirectories every 30 seconds for
// new or removed git repositories, registering and deregistering them without
// requiring a server restart.
func (s *Server) watchNewRepos() {
	mtimes := make(map[string]time.Time)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.pollRepoChanges(s.ctx, mtimes)
		case <-s.ctx.Done():
			return
		}
	}
}

// pollRepoChanges enumerates all directories at depth 0..repoDiscoveryDepth-1
// from absRoot, stats each, and for those whose mtime has advanced calls
// syncReposInDir. Directories that have disappeared since the last tick
// trigger deregisterReposUnder.
func (s *Server) pollRepoChanges(ctx context.Context, mtimes map[string]time.Time) {
	dirs := collectWatchDirs(ctx, s.absRoot, repoDiscoveryDepth-1)
	dirSet := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		dirSet[d] = struct{}{}
	}
	// Deregister repos under any directory that disappeared since last tick.
	for dir := range mtimes {
		if _, ok := dirSet[dir]; !ok {
			delete(mtimes, dir)
			s.deregisterReposUnder(ctx, dir)
		}
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			delete(mtimes, dir)
			s.deregisterReposUnder(ctx, dir)
			continue
		}
		if !info.ModTime().After(mtimes[dir]) {
			continue
		}
		mtimes[dir] = info.ModTime()
		s.syncReposInDir(ctx, dir)
	}
}

// syncReposInDir discovers repositories at depth 1 within dir and reconciles
// the registered set: new repos are initialised and removed repos are
// deregistered.
func (s *Server) syncReposInDir(ctx context.Context, dir string) {
	paths, err := gitutil.DiscoverRepos(dir, 1)
	if err != nil {
		slog.WarnContext(ctx, "discover repos: scan failed", "dir", dir, "err", err)
		return
	}
	currentSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		currentSet[p] = struct{}{}
	}

	// Deregister repos directly under dir that are no longer present.
	s.mu.Lock()
	var removed []string
	s.repos = slices.DeleteFunc(s.repos, func(r repoInfo) bool {
		if filepath.Dir(r.AbsPath) != dir {
			return false
		}
		if _, ok := currentSet[r.AbsPath]; ok {
			return false
		}
		removed = append(removed, r.RelPath)
		delete(s.runners, r.RelPath)
		return true
	})
	registered := make(map[string]struct{}, len(s.repos))
	for i := range s.repos {
		registered[s.repos[i].AbsPath] = struct{}{}
	}
	s.mu.Unlock()
	for _, rel := range removed {
		slog.InfoContext(ctx, "deregistered removed repo", "path", rel)
	}

	// Initialise newly discovered repos in parallel.
	var newPaths []string
	for _, p := range paths {
		if _, ok := registered[p]; !ok {
			newPaths = append(newPaths, p)
		}
	}
	if len(newPaths) == 0 {
		return
	}
	results := make([]repoInitResult, len(newPaths))
	var wg sync.WaitGroup
	for i, abs := range newPaths {
		wg.Go(func() {
			rel, err := filepath.Rel(s.absRoot, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			// Guard against a concurrent clone adding the same path.
			s.mu.Lock()
			_, exists := s.runners[rel]
			s.mu.Unlock()
			if exists {
				return
			}
			remoteName, err := gitutil.DefaultRemote(ctx, abs)
			if err != nil {
				slog.DebugContext(ctx, "new repo: no remote, skipping", "path", abs, "err", err)
				return
			}
			branch, err := gitutil.DefaultBranch(ctx, abs, remoteName)
			if err != nil {
				slog.WarnContext(ctx, "new repo: cannot determine default branch", "path", abs, "err", err)
				return
			}
			remote := gitutil.RemoteOriginURL(ctx, abs)
			runner := &task.Runner{
				BaseBranch: branch,
				Dir:        abs,
				LogDir:     s.logDir,
				CacheDir:   s.cacheDir,
				HarnessEnv: s.backend.HarnessEnv,
				Container:  s.backend,
			}
			if err := runner.Init(ctx); err != nil {
				slog.WarnContext(ctx, "new repo: runner init failed", "path", abs, "err", err)
			}
			var forgeKind forge.Kind
			var forgeOwner, forgeRepo string
			if rawURL, err := forge.RemoteURL(ctx, abs); err == nil {
				forgeKind, forgeOwner, forgeRepo, _ = forge.ParseRemoteURL(rawURL)
			}
			results[i] = repoInitResult{
				info: repoInfo{
					RelPath: rel, AbsPath: abs, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote,
					ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo,
				},
				runner: runner,
			}
			slog.InfoContext(ctx, "discovered new repo", "path", rel, "br", branch)
		})
	}
	wg.Wait()

	s.mu.Lock()
	for i := range results {
		if results[i].runner == nil {
			continue
		}
		rel := results[i].info.RelPath
		if _, exists := s.runners[rel]; exists {
			continue
		}
		s.repos = append(s.repos, results[i].info)
		s.runners[rel] = results[i].runner
	}
	s.mu.Unlock()
}

// deregisterReposUnder removes all registered repositories whose absolute
// path is within dir (used when dir itself has been deleted).
func (s *Server) deregisterReposUnder(ctx context.Context, dir string) {
	prefix := dir + string(filepath.Separator)
	s.mu.Lock()
	var removed []string
	s.repos = slices.DeleteFunc(s.repos, func(r repoInfo) bool {
		if !strings.HasPrefix(r.AbsPath, prefix) {
			return false
		}
		removed = append(removed, r.RelPath)
		delete(s.runners, r.RelPath)
		return true
	})
	s.mu.Unlock()
	for _, rel := range removed {
		slog.InfoContext(ctx, "deregistered removed repo", "path", rel)
	}
}

// collectWatchDirs returns root and all of its subdirectories down to
// maxDepth levels, for use as mtime-watch targets. Subdirectories that
// cannot be read are silently skipped. Dot-prefixed entries (e.g. ".git")
// are skipped to match gitutil.DiscoverRepos's recursion behaviour and to
// avoid descending into a repo's internal .git directory, which itself
// looks like a bare repo to the discoverer.
func collectWatchDirs(ctx context.Context, root string, maxDepth int) []string {
	dirs := []string{root}
	if maxDepth <= 0 {
		return dirs
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		slog.DebugContext(ctx, "watch dirs: read dir failed", "path", root, "err", err)
		return dirs
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(root, e.Name())
		dirs = append(dirs, collectWatchDirs(ctx, sub, maxDepth-1)...)
	}
	return dirs
}

// detectProviders scans harness environment variables for known API keys and
// OAuth credential files, creating the appropriate ProviderFetcher for each
// provider found. OAuth-based providers (Anthropic, Codex) are always
// attempted since their credentials come from files, not env vars.
func detectProviders(ctx context.Context, harnessEnv map[string][]string) []usage.ProviderFetcher {
	// Collect all env vars across all harnesses.
	envKeys := make(map[string]struct{})
	for _, envs := range harnessEnv {
		for _, e := range envs {
			if k, _, ok := strings.Cut(e, "="); ok {
				envKeys[k] = struct{}{}
			}
		}
	}

	var fetchers []usage.ProviderFetcher

	// OAuth-based: always try these (they watch credential files).
	if f := usage.NewAnthropicFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}
	if f := usage.NewCodexFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}

	// API-key-based: detect from env vars.
	if _, ok := envKeys["DEEPSEEK_API_KEY"]; ok {
		key := firstEnvValue(harnessEnv, "DEEPSEEK_API_KEY")
		if f := usage.NewDeepSeekFetcher(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}
	if _, ok := envKeys["OPENROUTER_API_KEY"]; ok {
		key := firstEnvValue(harnessEnv, "OPENROUTER_API_KEY")
		if f := usage.NewOpenRouterFetcher(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}

	slog.InfoContext(ctx, "provider usage fetchers", "count", len(fetchers))
	return fetchers
}

// firstEnvValue returns the value for the given key from the first harness
// that defines it.
func firstEnvValue(harnessEnv map[string][]string, key string) string {
	for _, envs := range harnessEnv {
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if ok && k == key {
				return v
			}
		}
	}
	return ""
}

// autoDetectLLMProvider detects the best available LLM provider from the
// genai providers registry by attempting to instantiate and ping each one.
// It prefers locally-available providers (codex, opencode, claudecode) over
// remote APIs (gemini). Returns "" if no suitable provider is found.
func autoDetectLLMProvider(ctx context.Context, geminiAPIKey string) string {
	// Preferred order: container-local providers first, then others.
	preferred := []string{
		"codex",
		"opencode",
		"claudecode",
		"gemini",
	}
	for _, name := range preferred {
		if pingProvider(ctx, name, geminiAPIKey) {
			return name
		}
	}
	// Fallback: iterate over all providers and pick the first one that responds to ping.
	for name := range providers.All {
		if pingProvider(ctx, name, geminiAPIKey) {
			return name
		}
	}
	return ""
}

// pingProvider attempts to instantiate and ping a provider, returning true if successful.
func pingProvider(ctx context.Context, name, geminiAPIKey string) bool {
	c, ok := providers.All[name]
	if !ok || c.Factory == nil {
		return false
	}
	var opts []genai.ProviderOption
	opts = append(opts, genai.ModelCheap)
	// Pass API key if configured for the provider.
	if name == "gemini" && geminiAPIKey != "" {
		opts = append(opts, genai.ProviderOptionAPIKey(geminiAPIKey))
	}
	p, err := c.Factory(ctx, opts...)
	if err != nil {
		slog.Debug("provider factory failed", "prov", name, "err", err)
		return false
	}
	// If the provider supports pinging, verify it's accessible.
	if pinger, ok := p.(genai.ProviderPing); ok {
		if err := pinger.Ping(ctx); err != nil {
			slog.Debug("provider ping failed", "prov", name, "err", err)
			return false
		}
	}
	slog.Info("provider detected", "prov", name)
	return true
}
