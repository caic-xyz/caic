// Validates task preconditions and incrementally scans Git diffs for unsafe content.

package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// SafetyIssue describes a potential problem detected before pushing to origin.
type SafetyIssue struct {
	File   string
	Kind   string // "large_binary" or "secret"
	Detail string // Human-readable description.
}

// maxBinarySize is the threshold above which a binary file triggers a warning.
const maxBinarySize = 500 * 1024 // 500 KB

// secretPatterns are compiled regexps that match common secret material in diff
// added lines. Pattern strings are split so they don't match themselves.
var secretPatterns = []*secretPattern{
	{regexp.MustCompile(`AK` + `IA[0-9A-Z]{16}`), "AWS access key"},
	{regexp.MustCompile(`-{5}` + `BEGIN\s+(RSA|DSA|EC|OPENSSH|PGP)\s+PRIV` + `ATE\s+KEY-{5}`), "private key"},
	{regexp.MustCompile(`gh` + `p_[A-Za-z0-9_]{36}`), "GitHub personal access token"},
	{regexp.MustCompile(`gh` + `o_[A-Za-z0-9_]{36}`), "GitHub OAuth token"},
	{regexp.MustCompile(`github` + `_pat_[A-Za-z0-9_]{22,}`), "GitHub fine-grained PAT"},
	{regexp.MustCompile(`sk` + `-[A-Za-z0-9]{20,}`), "API secret key"},
	{regexp.MustCompile(`(?i)(pass` + `word|sec` + `ret|to` + `ken|api[_-]?key)\s*[:=]\s*['"][^'"]{8,}`), "hardcoded credential"},
}

type secretPattern struct {
	re   *regexp.Regexp
	desc string
}

// CheckSafety scans the diff for large binary files and potential secrets.
// It returns any issues found. A non-nil error indicates a git command failure,
// or incomplete secret scan, not a safety problem.
func CheckSafety(ctx context.Context, log *slog.Logger, dir, branch, baseBranch string, ds v3.DiffStat) ([]SafetyIssue, error) {
	if log == nil {
		return nil, errors.New("safety logger is required")
	}
	var issues []SafetyIssue

	// Check binary file sizes.
	for _, f := range ds {
		if !f.Binary {
			continue
		}
		size, err := gitCatFileSize(ctx, log, dir, branch, f.Path)
		if err != nil {
			// File may have been deleted; skip.
			continue
		}
		if size > maxBinarySize {
			issues = append(issues, SafetyIssue{
				File:   f.Path,
				Kind:   "large_binary",
				Detail: fmt.Sprintf("binary file is %s (limit %s)", humanSize(size), humanSize(maxBinarySize)),
			})
		}
	}

	// Scan added lines for secrets.
	secretIssues, err := scanDiffForSecrets(ctx, log, dir, branch, baseBranch)
	if err != nil {
		return issues, err
	}
	issues = append(issues, secretIssues...)
	return issues, nil
}

// gitCatFileSize returns the size of a blob in the given branch.
func gitCatFileSize(ctx context.Context, log *slog.Logger, dir, branch, path string) (int64, error) {
	log.DebugContext(ctx, "git cat-file size", "branch", branch, "path", path)
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-s", branch+":"+path) //nolint:gosec // branch and path are from internal git state, not user input.
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// scanDiffForSecrets runs git diff and scans added lines for secret patterns.
func scanDiffForSecrets(ctx context.Context, log *slog.Logger, dir, branch, baseBranch string) ([]SafetyIssue, error) {
	log.InfoContext(ctx, "git diff for secrets", "branch", branch, "baseBranch", baseBranch)
	// Cancel the producer on scan failure, and always reap it before returning.
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", "diff", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "origin/"+baseBranch+"..."+branch) //nolint:gosec // branch names are from internal git state.
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	// A configured diff helper can emit credentials or source on stderr.
	// Drain it without retaining bytes that could escape through returned errors.
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("git diff stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git diff for secret scan: %w", err)
	}
	// An external diff helper can inherit stdout after Git is killed. Closing
	// the read side on cancellation releases a blocked scan in that case.
	stopClose := context.AfterFunc(ctx, func() {
		if err := stdout.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			log.DebugContext(ctx, "close cancelled secret scan", "err", err)
		}
	})
	defer stopClose()

	var issues []SafetyIssue
	seen := make(map[string]struct{}) // dedupe by file+kind
	var currentFile string

	// Regexps consume one complete line. Bound that working set at 1 MiB;
	// larger lines fail the scan rather than silently omitting subsequent secrets.
	const maxLineSize = 1 << 20
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineSize+2)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			cancel()
			return nil, errors.Join(err, cmd.Wait())
		}
		line := scanner.Bytes()
		if len(line) > maxLineSize {
			cancel()
			return nil, errors.Join(errors.New("secret scan diff line exceeds 1 MiB"), cmd.Wait())
		}
		// Track current file from diff headers.
		if after, ok := bytes.CutPrefix(line, []byte("+++ b/")); ok {
			currentFile = string(after)
			continue
		}
		// Only scan added lines.
		if !bytes.HasPrefix(line, []byte("+")) || bytes.HasPrefix(line, []byte("+++")) {
			continue
		}
		added := line[1:]
		for _, sp := range secretPatterns {
			if !sp.re.Match(added) {
				continue
			}
			key := currentFile + ":" + sp.desc
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			log.WarnContext(ctx, "secret pattern matched", "file", currentFile, "pattern", sp.desc)
			issues = append(issues, SafetyIssue{
				File:   currentFile,
				Kind:   "secret",
				Detail: fmt.Sprintf("possible %s detected", sp.desc),
			})
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		cancel()
		scanErr = fmt.Errorf("secret scan diff line (maximum 1 MiB): %w", scanErr)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("git diff for secret scan: %w", waitErr)
	}
	if err := errors.Join(scanErr, waitErr, ctx.Err()); err != nil {
		return nil, err
	}
	return issues, nil
}

// humanSize formats bytes as a human-readable string.
func humanSize(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.0f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
