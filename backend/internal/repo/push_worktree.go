// Pools completed push worktrees across server restarts and expires idle checkouts.

package repo

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caic-xyz/md/git"
	"github.com/gofrs/flock"
)

const pushWorktreeIdleTTL = 24 * time.Hour

var errInvalidPushMarker = errors.New("invalid push ownership marker")

type idlePushWorktree struct {
	dir   string
	since time.Time
}

// SweepPushWorktrees expires confirmed idle checkouts across the configured
// application cache, including repositories no longer in the discovery set.
//
// Interrupted and legacy operations remain quarantined. The same pool locks
// protect startup sweeps, periodic sweeps, and pushes across server processes.
func SweepPushWorktrees(ctx context.Context, log *slog.Logger, cacheDir string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cache, err := pushCacheDir(cacheDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	var errs []error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "operation-") {
			continue
		}
		dir := filepath.Join(cache, entry.Name())
		since, err := readPushIdleSince(dir)
		if err != nil || time.Since(since) < pushWorktreeIdleTTL {
			continue
		}
		owner, err := readPushMarker(dir, "owner-v1")
		if err != nil || !filepath.IsAbs(owner) {
			continue
		}
		g := &git.Checkout{Root: owner, Logger: log}
		common, err := pushCommonDir(ctx, g)
		if err != nil {
			errs = append(errs, fmt.Errorf("expire push cache %s: %w", dir, err))
			continue
		}
		if _, ok := seen[common]; ok {
			continue
		}
		seen[common] = struct{}{}
		if err := withPushPool(ctx, cache, common, func() error {
			return expireIdlePushWorktrees(ctx, g, cache, common)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pushCacheDir owns the cache layout and canonicalization. Only checkout
// construction creates it; a background sweep treats a missing cache as empty.
func pushCacheDir(dir string, create bool) (string, error) {
	p, err := filepath.Abs(filepath.Join(dir, "push"))
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(p, 0o700); err != nil { //nolint:gosec // configured application cache root, not request input.
			return "", err
		}
	}
	return filepath.EvalSymlinks(p)
}

// The persistent lock is outside operation directories and is never removed.
// All servers sharing this cache serialize claiming, publishing, and expiring
// idle worktrees. Removing idle-v1 claims a checkout before any Git mutation;
// a crash leaves no reusable marker, even after the OS releases this lock.
func withPushPool(ctx context.Context, cache, common string, run func() error) (err error) {
	dir := filepath.Join(cache, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // configured application push cache, not request input.
		return err
	}
	lock := flock.New(filepath.Join(dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(common)))))
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return ctx.Err()
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}

func readPushMarker(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path) //nolint:gosec // bounded marker in the configured application cache.
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", errInvalidPushMarker
	}
	data, err := os.ReadFile(path) //nolint:gosec // bounded regular marker in configured cache.
	return string(data), err
}

func idlePushWorktrees(cache, common string) ([]idlePushWorktree, error) {
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil, err
	}
	var idle []idlePushWorktree
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "operation-") {
			continue
		}
		dir := filepath.Join(cache, entry.Name())
		owner, err := readPushMarker(dir, "owner-v1")
		if err != nil || owner != common {
			continue
		}
		since, err := readPushIdleSince(dir)
		if err == nil {
			idle = append(idle, idlePushWorktree{dir: dir, since: since})
		}
	}
	return idle, nil
}

func readPushIdleSince(dir string) (time.Time, error) {
	stamp, err := readPushMarker(dir, "idle-v1")
	if err != nil {
		return time.Time{}, err
	}
	if !strings.HasSuffix(stamp, "\n") {
		return time.Time{}, errors.New("incomplete push idle marker")
	}
	return time.Parse(time.RFC3339Nano, strings.TrimSuffix(stamp, "\n"))
}

// Claim before cleanup, too: a timed-out Git removal must never leave a ready
// checkout that another server could reuse while removal continues.
func discardIdlePushWorktree(ctx context.Context, g *git.Checkout, common, dir string) error {
	if err := os.Remove(filepath.Join(dir, "idle-v1")); err != nil {
		return err
	}
	return finishPushCleanup(ctx, dir, func() error {
		return removePushWorktree(ctx, g, common, dir)
	})
}

func expireIdlePushWorktrees(ctx context.Context, g *git.Checkout, cache, common string) error {
	idle, err := idlePushWorktrees(cache, common)
	if err != nil {
		return err
	}
	for _, slot := range idle {
		if time.Since(slot.since) < pushWorktreeIdleTTL {
			continue
		}
		if err := discardIdlePushWorktree(ctx, g, common, slot.dir); err != nil {
			return errors.Join(err, retainPushWorktree(ctx, g.Logger, g.Root, slot.dir))
		}
	}
	return nil
}

func acquirePushWorktree(ctx context.Context, cache, common string) (dir string, reused bool, err error) {
	err = withPushPool(ctx, cache, common, func() error {
		idle, err := idlePushWorktrees(cache, common)
		if err != nil {
			return err
		}
		if len(idle) != 0 {
			dir = idle[0].dir
			reused = true
			return os.Remove(filepath.Join(dir, "idle-v1"))
		}
		dir, err = os.MkdirTemp(cache, "operation-")
		if err != nil {
			return err
		}
		// Canonical repository identity survives source-checkout aliases. Older
		// root-path ownership markers remain readable; neither form is rewritten.
		if err := os.WriteFile(filepath.Join(dir, "owner-v1"), []byte(common), 0o600); err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
		return nil
	})
	return dir, reused, err
}

func releasePushWorktree(ctx context.Context, g *git.Checkout, cache, common, dir string, reusable bool) error {
	return withPushPool(ctx, cache, common, func() error {
		if reusable {
			if err := awaitPushCommand(ctx, func() error {
				return validateReusablePushWorktree(ctx, g, common, dir)
			}); err != nil {
				return err
			}
			idle, err := idlePushWorktrees(cache, common)
			if err != nil {
				return err
			}
			// Keep at most one idle checkout; concurrent excess slots are removed.
			if len(idle) == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				stamp := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
				return os.WriteFile(filepath.Join(dir, "idle-v1"), []byte(stamp), 0o600) //nolint:gosec // exclusively claimed operation directory under the configured cache.
			}
		}
		return finishPushCleanup(ctx, dir, func() error {
			return removePushWorktree(ctx, g, common, dir)
		})
	})
}

func pushCommonDir(ctx context.Context, g *git.Checkout) (string, error) {
	dir, err := g.RunGit(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(dir))
}

// awaitPushCommand bounds the caller even when Git's captured output pipes are
// still held by a hook descendant. A buffered result lets Git finish afterwards;
// the caller retains all operation resources on cancellation.
func awaitPushCommand(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finishPushCleanup leaves directory removal to the waiting caller. A cleanup
// command completing after its deadline cannot erase retained ownership in the
// background. Once command completion is observed, removal and its reported
// outcome stay together, even if the context expires during filesystem cleanup.
func finishPushCleanup(ctx context.Context, dir string, run func() error) error {
	if err := awaitPushCommand(ctx, run); err != nil {
		return err
	}
	return os.RemoveAll(dir) //nolint:gosec // owned operation directory after validated Git cleanup completes.
}

func retainPushWorktree(ctx context.Context, log git.Logger, root, dir string) error {
	path := filepath.Join(dir, "checkout")
	err := fmt.Errorf("push operation retained at %q; after confirming its Git and hook processes have stopped, run git -C %q worktree remove --force %q, then remove operation directory %q", dir, root, path, dir)
	log.Log(ctx, slog.LevelWarn, "Retained push operation requires manual cleanup", "operation", dir, "guidance", err.Error())
	return err
}

func inspectPushWorktree(ctx context.Context, g *git.Checkout, common, dir string) (bool, error) {
	path := filepath.Join(dir, "checkout")
	info, err := os.Lstat(path) //nolint:gosec // owned operation checkout path under the configured cache; replacements are rejected below.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil && !info.IsDir() {
		return false, errors.New("refusing a replaced push checkout")
	}
	out, err := g.RunGit(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	registered := false
	for block := range strings.SplitSeq(out, "\x00\x00") {
		fields := strings.Split(block, "\x00")
		registeredPath, ok := strings.CutPrefix(fields[0], "worktree ")
		if !ok || filepath.Clean(filepath.FromSlash(registeredPath)) != filepath.Clean(path) {
			continue
		}
		registered = true
		if !strings.Contains(block, "\x00detached\x00") && !strings.HasSuffix(block, "\x00detached") {
			return false, errors.New("refusing a non-detached push worktree")
		}
		if strings.Contains(block, "\x00locked") {
			return false, errors.New("refusing a locked push worktree")
		}
	}
	if registered {
		// Also validate the live checkout's common directory when it exists. A
		// missing checkout after a crash is safe to remove through its registration.
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil { //nolint:gosec // owned cache checkout pointer inspected before Git discovery.
			actual, err := pushCommonDir(ctx, &git.Checkout{Root: path, Logger: g.Logger})
			if err != nil {
				return false, err
			}
			if actual != common {
				return false, errors.New("push worktree ownership changed")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	} else if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil { //nolint:gosec // owned cache checkout pointer inspected before Git discovery.
		return false, errors.New("refusing an unregistered Git checkout")
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return registered, nil
}

func removePushWorktree(ctx context.Context, g *git.Checkout, common, dir string) error {
	registered, err := inspectPushWorktree(ctx, g, common, dir)
	if err != nil {
		return err
	}
	if registered {
		if _, err := g.RunGit(ctx, "worktree", "remove", "--force", filepath.Join(dir, "checkout")); err != nil {
			return fmt.Errorf("remove push worktree: %w", err)
		}
	}
	return nil
}

func validateReusablePushWorktree(ctx context.Context, g *git.Checkout, common, dir string) error {
	registered, err := inspectPushWorktree(ctx, g, common, dir)
	if err != nil {
		return err
	}
	if !registered {
		return errors.New("push worktree registration disappeared")
	}
	// A missing .git is safe to remove through registration, but never safe to
	// run checkout-local clean/reset: Git could discover a parent repository.
	info, err := os.Lstat(filepath.Join(dir, "checkout", ".git")) //nolint:gosec // owned cache pointer must be regular before checkout reuse.
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid push worktree Git pointer")
	}
	return nil
}
