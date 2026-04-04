// Server startup: New() constructor, container adoption, and background maintenance.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
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
	logDir := cfg.CacheDir
	if logDir == "" {
		return nil, errors.New("CacheDir is required")
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

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
		paths, err := gitutil.DiscoverRepos(rootDir, 3)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		logs, err := task.LoadLogs(logDir)
		logCh <- logsResult{logs, err}
	}()
	go func() {
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

	backend := &container.Backend{Client: mdClient}

	cachePath := filepath.Join(cfg.CacheDir, "ci_results.json")
	cache, err := forgecache.Open(cachePath)
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "path", cachePath, "err", err)
		cache, _ = forgecache.Open("")
	}

	var voiceBridge *voicertc.Bridge
	if cfg.WebRTCPort > 0 {
		voiceBridge, err = voicertc.NewBridge(cfg.GeminiAPIKey, cfg.WebRTCPort)
		if err != nil {
			return nil, fmt.Errorf("voice bridge: %w", err)
		}
	}

	s := &Server{
		ctx:                ctx,
		absRoot:            absRoot,
		runners:            make(map[string]*task.Runner, len(repoRes.paths)),
		mdClient:           mdClient,
		logDir:             logDir,
		prefs:              prefsStore,
		authStore:          authStore,
		sessionSecret:      sessionSecret,
		githubOAuth:        githubOAuth,
		gitlabOAuth:        gitlabOAuth,
		githubAllowedUsers: githubAllowedUsers,
		gitlabAllowedUsers: gitlabAllowedUsers,
		hostState:          hostState,
		usage:              newUsageFetcher(ctx),
		pprof:              cfg.Pprof,
		geminiAPIKey:       cfg.GeminiAPIKey,
		voiceBridge:        voiceBridge,
		forge:              newForgeManager(cfg.GitHubToken, cfg.GitLabToken, nil),
		ciCache:            cache,
		backend:            backend,
		tasks:              make(map[string]*taskEntry),
		repoCIStatus:       make(map[string]repoCIState),
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

	if cfg.LLMProvider != "" {
		if c, ok := providers.All[cfg.LLMProvider]; !ok || c.Factory == nil {
			slog.Warn("unknown LLM provider for title generation", "prov", cfg.LLMProvider)
		} else {
			var opts []genai.ProviderOption
			if cfg.LLMModel != "" {
				opts = append(opts, genai.ProviderOptionModel(cfg.LLMModel))
			} else {
				opts = append(opts, genai.ModelCheap)
			}
			if p, err := c.Factory(ctx, opts...); err != nil {
				slog.Warn("LLM provider init failed", "prov", cfg.LLMProvider, "err", err)
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
	s.bot = bot.New(ctx, s)

	// Always register a no-repo runner (keyed by "") for tasks that don't
	// need a git repository.
	noRepoRunner := &task.Runner{LogDir: logDir, Container: backend}
	_ = noRepoRunner.Init(ctx) // populates Backends; no-op for no-repo (no branches to scan)
	s.runners[""] = noRepoRunner

	// Phase 3: Load purged tasks from pre-loaded logs.
	if logRes.err != nil {
		slog.Warn("load logs failed", "err", logRes.err)
	} else {
		if err := s.loadPurgedTasksFrom(logRes.logs); err != nil {
			return nil, fmt.Errorf("load purged tasks: %w", err)
		}
	}

	// Phase 4: Adopt containers (using pre-fetched list).
	if contRes.err != nil {
		slog.Warn("list containers failed, skipping adoption", "err", contRes.err)
	} else {
		if err := s.adoptContainers(ctx, contRes.containers, logRes.logs); err != nil {
			return nil, fmt.Errorf("adopt containers: %w", err)
		}
	}

	// Resume bot comment watchers for adopted tasks with pending forge issues.
	s.bot.ResumePendingComments()

	s.ipgeoChecker, err = ipgeo.NewChecker(ctx, cfg.IPGeoAllowlist, cfg.IPGeoDB)
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeoDB != "" {
		slog.Info("ipgeo", "path", cfg.IPGeoDB, "list", cfg.IPGeoAllowlist)
	}

	s.watchContainerEvents(ctx)
	go s.warmupImages()
	go s.pollStats(s.ctx) //nolint:contextcheck // server-lifetime context is intentional
	return s, nil
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
			Repos:         lt.Repos, // GitRoot is empty for purged tasks
			Harness:       lt.Harness,
			StartedAt:     lt.StartedAt,
			Tailscale:     lt.Tailscale,
			USB:           lt.USB,
			Display:       lt.Display,
		}
		t.SetStateAt(lt.State, lt.LastStateUpdateAt)
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		if err := lt.LoadMessages(); err != nil {
			ltPrimary := lt.Primary()
			ltRepo, ltBranch := "", ""
			if ltPrimary != nil {
				ltRepo = ltPrimary.Name
				ltBranch = ltPrimary.Branch
			}
			slog.Warn("load messages failed", "repo", ltRepo, "br", ltBranch, "err", err)
		}
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		// For tasks without a caic_result trailer (lt.State == StateRunning
		// sentinel), any state RestoreMessages inferred is unreliable — the
		// task may have been purged or interrupted without a trailer.
		// Force StateFailed; adoptContainers replaces this entry with the
		// correct live state if the container is still running.
		if lt.State == task.StateRunning {
			t.SetState(task.StateFailed)
		}
		// SetPR after LoadMessages: the header-only tail scan may miss
		// caic_pr when the record is beyond the 64 KiB window; the full
		// parse in LoadMessages always finds it.
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
		// Backfill result stats from restored messages when the trailer
		// has zero cost (e.g. session exited without a final ResultMessage).
		if lt.Result.CostUSD == 0 {
			lt.Result.CostUSD, lt.Result.NumTurns, lt.Result.Duration, lt.Result.Usage, _ = t.LiveStats()
		}
		done := make(chan struct{})
		close(done)
		entry := &taskEntry{task: t, result: lt.Result, done: done}
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
	claimed := make(map[string]bool, len(containers))

	for i := range s.repos {
		ri := &s.repos[i]
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, c := range containers {
			branch, ok := container.BranchFromContainer(c.Name, repoName)
			if !ok {
				continue
			}
			claimed[c.Name] = true
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
			if claimed[c.Name] || !strings.HasPrefix(c.Name, "md-agent-") {
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
	// Only adopt containers that caic started. The caic label is set at
	// container creation and is the authoritative proof of ownership.
	labelVal, err := container.LabelValue(ctx, c.Name, "caic")
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
	for i := len(allLogs) - 1; i >= 0; i-- {
		log := allLogs[i]
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
		relayAlive, relayDiag, relayErr = agent.RelayStatus(ctx, c.Name)
		if relayErr != nil {
			slog.Warn("relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr, "diag", relayDiag)
		}
		if relayAlive {
			// Relay is alive — read authoritative output from container.
			relayMsgs, relaySize, relayErr = runner.ReadRelayOutput(ctx, c.Name, harnessName)
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
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, GitRoot: ri.AbsPath, Branch: branch}}
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
	case ri.ForgeOwner != "" && branch != "" && ri.ForgeKind != "":
		// Query the forge for an existing PR created outside of caic.
		f := s.forge.forgeForInfo(ctx, &ri)
		if f == nil && s.authStore != nil {
			if u, ok := s.authStore.FindByProvider(ri.ForgeKind); ok {
				f = s.forge.forgeFor(auth.NewContext(ctx, &u), ri.ForgeKind)
			}
		}
		if f != nil {
			pr, err := f.FindPRByBranch(ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
			if err == nil && pr.Number > 0 {
				slog.Info("adopt: found external PR", "repo", ri.RelPath, "br", branch, "pr", pr.Number)
				t.SetPR(ri.ForgeOwner, ri.ForgeRepo, pr.Number)
			}
		}
	}

	// Restore messages from relay or logs.
	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss. It contains both
		// Claude Code stdout and user inputs (logged by the relay).
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Debug("relay", "msg", "restored from", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(relayMsgs))
	} else if lt != nil {
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
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
		if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			slog.Warn("relay", "msg", "dead, marking waiting",
				"repo", ri.RelPath, "br", branch, "ctr", c.Name,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	// Track whether we've already registered the task entry (happens for external PRs).
	entryRegistered := false
	entry := &taskEntry{task: t, done: make(chan struct{})}

	// Register entry and start CI monitoring if a PR was found (either from logs or external).
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
					go s.monitorCI(s.ctx, entry, f, ri.ForgeOwner, ri.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive the request
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

	// Auto-reconnect in background: relay alive → attach; relay dead
	// → restart relay via --resume (requires a session ID).
	// Skip reconnect for stopped tasks — container is not running.
	if t.GetState() != task.StateStopped && (relayAlive || t.GetSessionID() != "") {
		strategy := "attach"
		if !relayAlive {
			strategy = "resume"
		}
		slog.Debug("container", "msg", "auto-reconnect starting", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "st", strategy)
		go func() {
			tlog := slog.With("repo", ri.RelPath, "br", branch, "ctr", t.Container)
			h, err := runner.Reconnect(ctx, t, true)
			if err != nil {
				tlog.Warn("auto-reconnect failed", "st", strategy, "err", err)
				s.notifyTaskChange()
				return
			}
			// If --resume exits immediately (previous session complete),
			// start a fresh idle relay so the task can accept prompts.
			h, err = runner.EnsureSession(ctx, t, h, tlog)
			if err != nil {
				tlog.Warn("ensure session failed", "err", err)
				t.SetState(task.StateWaiting)
				s.notifyTaskChange()
				return
			}
			tlog.Debug("auto-reconnect succeeded", "st", strategy)
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
		slog.Warn("adopted orphaned task",
			"repo", ri.RelPath, "br", branch, "ctr", c.Name,
			"state", t.GetState())
	}
	return nil
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
