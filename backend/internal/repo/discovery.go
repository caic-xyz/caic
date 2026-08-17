// Checkout discovery and cloning operations for the repository domain.

package repo

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// DiscoverCheckout reads a local checkout's git configuration. The returned
// checkout has an unregistered repository identity when it has a remote.
// liveBranches are branch names taken from currently running containers
// mapped to dir; see NewCheckout.
func DiscoverCheckout(ctx context.Context, log *slog.Logger, dir string, liveBranches []string) (*Checkout, error) {
	gitCheckout := &git.Checkout{Root: dir, Logger: log}
	remoteName, err := gitCheckout.DefaultRemote(ctx)
	if err != nil {
		return nil, fmt.Errorf("determine default remote: %w", err)
	}
	branch, err := gitCheckout.DefaultBranch(ctx, remoteName)
	if err != nil {
		return nil, fmt.Errorf("determine default branch: %w", err)
	}
	remote := gitCheckout.RemoteOriginURL(ctx)
	checkout, err := NewCheckout(ctx, log, dir, branch, liveBranches)
	if err != nil {
		return nil, fmt.Errorf("initialize checkout: %w", err)
	}
	if remote != "" {
		kind, owner, name := parseForgeRemote(ctx, log, remote)
		checkout.Repository = &Repository{Remote: remote, ForgeKind: kind, ForgeOwner: owner, ForgeRepo: name}
	}
	checkout.BaseBranchRemote = remoteName
	return checkout, nil
}

// Clone creates a local git checkout at dir and discovers its configuration.
// It removes dir if cloning or discovery fails.
func Clone(ctx context.Context, log *slog.Logger, url, dir string, depth int) (*Checkout, error) {
	if depth == 0 {
		depth = 1
	}
	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	args := []string{"clone", "--depth", strconv.Itoa(depth), "--recurse-submodules", "--shallow-submodules", url, dir}
	cmd := exec.CommandContext(cloneCtx, "git", args...) //nolint:gosec // args are validated by the HTTP boundary except URL, which is user input to git.
	if out, err := cmd.CombinedOutput(); err != nil {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			return nil, fmt.Errorf("git clone failed: %w; remove incomplete checkout: %w", err, removeErr)
		}
		log.WarnContext(ctx, "git clone failed", "url", url, "err", err, "out", string(out))
		return nil, fmt.Errorf("git clone: %w", err)
	}
	// A freshly cloned repo can't yet have a running container mapped to it.
	checkout, err := DiscoverCheckout(ctx, log, dir, nil)
	if err == nil {
		return checkout, nil
	}
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		return nil, fmt.Errorf("discover checkout: %w; remove incomplete checkout: %w", err, removeErr)
	}
	return nil, fmt.Errorf("discover checkout: %w", err)
}

func parseForgeRemote(ctx context.Context, log *slog.Logger, remote string) (kind forge.Kind, owner, repoName string) {
	kind, owner, repoName, err := forge.ParseRemoteURL(remote)
	if err != nil {
		log.DebugContext(ctx, "unsupported forge remote", "remote", remote, "err", err)
	}
	return kind, owner, repoName
}
