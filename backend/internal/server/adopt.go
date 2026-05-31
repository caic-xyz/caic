// Container adoption on startup and runtime container-death handling.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/maruel/ksid"
)

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
		runner := s.runners[ri.RelPath]
		for _, c := range containers {
			// Match containers to repos via the md.repos Docker label.
			var branch, mountedName string
			var matched bool
			for _, r := range c.Repos {
				// MountedPath is the full absolute POSIX path inside
				// the container, e.g. "/home/user/src/github/caic".
				// TODO(2026-07-01): once all old containers lacking
				// mounted_path in their md.repos label have cycled
				// out, remove the basenamePath and pure-basename
				// fallbacks and keep only the fullPath match.
				fullPath := "/home/user/src/" + ri.RelPath
				basenamePath := "/home/user/src/" + filepath.Base(ri.AbsPath)
				if r.MountedPath == fullPath || r.MountedPath == ri.RelPath || r.MountedPath == basenamePath {
					branch = r.Branch
					mountedName = r.MountedPath
					matched = true
					break
				}
				// Pure basename fallback: buggy containers where
				// MountedPath was redundantly set to the basename.
				if r.MountedPath == filepath.Base(ri.AbsPath) {
					branch = r.Branch
					mountedName = r.MountedPath
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			claimed[c.Name] = struct{}{}
			wg.Go(func() {
				if err := s.adoptOne(ctx, *ri, runner, c, branch, mountedName, branchIDs, allLogs); err != nil {
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
				if err := s.adoptOne(ctx, repoInfo{}, noRepoRunner, c, "", "", branchIDs, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}

	// Warn about caic-labelled containers that weren't matched to any repo.
	// This catches MountedPath mismatches (e.g. old containers using
	// mounted_name basenames that don't match the expected full path).
	for _, c := range containers {
		if _, ok := claimed[c.Name]; ok {
			continue
		}
		if !strings.HasPrefix(c.Name, "md-") {
			continue
		}
		labelVal, err := container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic.id")
		if labelVal == "" && err == nil {
			labelVal, _ = container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic")
		}
		if labelVal != "" {
			slog.Error("adopt: caic not matched to any repo",
				"rt", s.mdClient.Runtime, "ctr", c.Name, "label", labelVal, "nrepos", len(c.Repos))
			for _, r := range c.Repos {
				slog.Error("adopt: unmatched repo detail",
					"rt", s.mdClient.Runtime, "ctr", c.Name, "mounted_path", r.MountedPath, "branch", r.Branch)
			}
		}
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
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch, mountedName string, branchIDs map[string][]string, allLogs []*task.LoadedTask) error { //nolint:gocritic // repoInfo size increase from GitHub fields; refactor not worth it
	ctx, adoptTask := trace.NewTask(ctx, "adopt-container")
	defer adoptTask.End()
	trace.Logf(ctx, "container", "%s repo=%s branch=%s", c.Name, ri.RelPath, branch)

	// Only adopt containers that caic started. The caic.id label is set at
	// container creation and is the authoritative proof of ownership.
	caicLabelReg := trace.StartRegion(ctx, "caic-label")
	trace.Logf(ctx, "adopt", "%s: label-caic", c.Name)
	labelVal, err := container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic.id")
	// TODO(2026-06-25): remove fallback to old "caic" label after all old containers have been
	// cycled. All newly launched containers use "caic.id" exclusively.
	if labelVal == "" && err == nil {
		labelVal, err = container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic")
	}
	caicLabelReg.End()
	if err != nil {
		return fmt.Errorf("label check for %s: %w", c.Name, err)
	}
	if labelVal == "" {
		slog.Info("skipping non-caic", "rt", s.mdClient.Runtime, "repo", ri.RelPath, "ctr", c.Name, "br", branch)
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
		slog.Info("adopting exited as stopped", "rt", s.mdClient.Runtime, "ctr", c.Name, "br", branch)
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
	harnessLabel, _ := container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic.harness")
	// TODO: remove fallback to old "harness" label after 2026-06-25.
	if harnessLabel == "" {
		harnessLabel, _ = container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "harness")
	}
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
		relayCtx, relayCancel := context.WithTimeout(ctx, 30*time.Second)
		relayAlive, relayDiag, relayErr = agent.RelayStatus(relayCtx, c.Name)
		relayCancel()
		relayStatusReg.End()
		if relayErr != nil {
			slog.Warn("relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr, "diag", relayDiag)
		}
		if relayAlive {
			// Relay is alive — read only the tail of the output for message
			// restoration. Files can be multi-GB; reading the entire thing
			// would block startup for minutes and OOM the process.
			relayReadReg := trace.StartRegion(ctx, "relay-read")
			trace.Logf(ctx, "adopt", "%s: relay-read", c.Name)
			readCtx, readCancel := context.WithTimeout(ctx, 2*time.Minute)
			if b := runner.Backends[harnessName]; b != nil {
				// NewParser synthesizes the trailing ResultMessage (stateful wire
				// format) — RestoreMessages needs it to infer the terminal
				// (waiting/asking) state instead of leaving the task stuck as
				// "running".
				relayMsgs, relaySize, relayErr = agent.ReadRelayTail(readCtx, c.Name, b.NewWire().ParseMessage, 10<<20) // 10 MiB tail
			} else {
				relaySize, relayErr = agent.RelayOutputSize(readCtx, c.Name)
			}
			readCancel()
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
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, BaseBranch: primaryBaseBranch, GitRoot: ri.AbsPath, Branch: branch, MountedPath: mountedName}}
		// Build a lookup of container repos by branch so secondary repos
		// use the real MountedPath from the Docker label, not a guess.
		containerRepoByBranch := make(map[string]string, len(c.Repos))
		for _, cr := range c.Repos {
			containerRepoByBranch[cr.Branch] = cr.MountedPath
		}
		if lt != nil {
			for _, lm := range lt.Repos[1:] {
				gitRoot := ""
				if er, ok := s.runners[lm.Name]; ok {
					gitRoot = er.Dir
				}
				mp := containerRepoByBranch[lm.Branch]
				if mp == "" {
					mp = "~/src/" + lm.Name
				}
				adoptRepos = append(adoptRepos, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, MountedPath: mp})
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
		Sudo:          c.Sudo,
		VNCPort:       int(c.VNCPort),
		Provider:      s.provider,
		ForgeIssue:    forgeIssue,
	}
	// Restore GitHub token flag from log trailer (primary) or container label (fallback).
	gtLabel, _ := container.LabelValue(ctx, s.mdClient.Runtime, c.Name, "caic.githubToken")
	if (lt != nil && lt.GitHubToken) || gtLabel == "true" {
		t.GitHubToken = true
	}
	if c.Sudo {
		if pw, err := c.SudoPassword(ctx); err == nil {
			t.SudoPassword = pw
		}
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
	if relayAlive {
		// Always set the relay offset so attach resumes from the right place.
		t.RelayOffset = relaySize
		if len(relayMsgs) > 0 {
			t.RestoreMessages(relayMsgs)
			slog.Debug("relay", "msg", "restored from relay", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(relayMsgs))
		}
	}
	// Fall back to log messages only when relay is dead AND the log is
	// small enough to parse quickly. Huge logs (multi-GB) would block
	// startup for minutes; messages load lazily when the user opens the task.
	const maxAdoptLogSize = 100 << 20 // 100 MiB
	if !relayAlive || len(relayMsgs) == 0 {
		if lt != nil && lt.LogSize <= maxAdoptLogSize {
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
		} else if lt != nil {
			slog.Warn("relay", "msg", "skipping log restore, file too large", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "size", lt.LogSize)
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
	if lt != nil && t.GetPR() == 0 && lt.LogSize <= maxAdoptLogSize {
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
			// Start CI monitoring in background — GetDefaultBranchSHA is a
			// forge API call that can block; must not stall adoption.
			pr := t.Snapshot().ForgePR
			if pr > 0 {
				go func() {
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
				}()
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

	slog.Info("adopted",
		"rt", s.mdClient.Runtime, "repo", ri.RelPath, "ctr", c.Name, "br", branch,
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
		slog.Debug("auto-reconnect starting", "rt", s.mdClient.Runtime, "repo", ri.RelPath, "br", branch, "ctr", c.Name)
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
			if ds := runner.BranchDiffStat(ctx, t.MDRepos()); len(ds) > 0 {
				t.SetLiveDiffStat(ds)
			}
			s.notifyTaskChange()
			s.watchSession(entry, runner, h)
		}()
	} else if !relayAlive && t.GetState() != task.StateStopped {
		slog.Error("relay dead, stopping",
			"rt", s.mdClient.Runtime, "repo", ri.RelPath, "br", branch, "ctr", c.Name,
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

// watchContainerEvents starts a single goroutine that listens for
// container die events and triggers cleanup for the corresponding task.
func (s *Server) watchContainerEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := container.WatchEvents(ctx, s.mdClient.Runtime, "caic")
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("events failed, retrying in 5s", "rt", s.mdClient.Runtime, "err", err)
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
			slog.Warn("events stream ended, reconnecting in 5s", "rt", s.mdClient.Runtime)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
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
	slog.Info("died, archiving as stopped", "rt", s.mdClient.Runtime, "ctr", containerName, "task", t.ID, "br", deathBranch, "prev_state", state)
	// Detach any active session (SSH is dead).
	t.DetachSession()
	t.SetState(task.StateStopped)
	s.notifyTaskChange()
}
