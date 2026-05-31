// Server startup: New() constructor and purged-task loading from on-disk logs.

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
	mdClient, err := container.New(cfg.TailscaleAPIKey, cfg.GitHubToken, cfg.Runtime)
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
				RepoName:   rel,
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

	// Warn on repo basename collisions. Containers use qualified names
	// (mountedName) so there is no runtime conflict, but users may be
	// confused by short basenames appearing in container names.
	{
		seen := make(map[string]string) // basename → first RelPath
		for i := range s.repos {
			ri := &s.repos[i]
			bn := filepath.Base(ri.AbsPath)
			if first, exists := seen[bn]; exists {
				slog.Warn("repo basename collision; containers will use qualified names",
					"a", first, "b", ri.RelPath, "basename", bn)
			} else {
				seen[bn] = ri.RelPath
			}
		}
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
		slog.Warn("list failed, skipping adoption", "rt", s.mdClient.Runtime, "err", contRes.err)
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

	s.ipgeoChecker, err = ipgeo.NewChecker(ctx, cfg.IPGeoAllowlist, cfg.IPGeoDB, "")
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeoDB != "" {
		slog.Info("ipgeo", "path", cfg.IPGeoDB, "list", cfg.IPGeoAllowlist)
	}

	s.watchContainerEvents(ctx)
	if !cfg.SkipWarmup {
		go func() {
			_, tk := trace.NewTask(ctx, "warmup-images")
			defer tk.End()
			trace.Log(ctx, "startup", "warmup-images: begin")
			s.warmupImages()
		}()
	}
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
			lt.SetParser(b.NewWire().ParseMessage)
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
		if err := entry.loadedTask.LoadMessagesTail(); err != nil {
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
		if lt.GitHubToken {
			t.GitHubToken = true
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
