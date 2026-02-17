package task

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/maruel/caic/backend/internal/agent"
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
// not a safety problem.
func CheckSafety(ctx context.Context, dir, branch, baseBranch string, ds agent.DiffStat) ([]SafetyIssue, error) {
	var issues []SafetyIssue

	// Check binary file sizes.
	for _, f := range ds {
		if !f.Binary {
			continue
		}
		size, err := gitCatFileSize(ctx, dir, branch, f.Path)
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
	secretIssues, err := scanDiffForSecrets(ctx, dir, branch, baseBranch)
	if err != nil {
		return issues, err
	}
	issues = append(issues, secretIssues...)
	return issues, nil
}

// gitCatFileSize returns the size of a blob in the given branch.
func gitCatFileSize(ctx context.Context, dir, branch, path string) (int64, error) {
	slog.Debug("git cat-file size", "branch", branch, "path", path)
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-s", branch+":"+path) //nolint:gosec // branch and path are from internal git state, not user input.
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// scanDiffForSecrets runs git diff and scans added lines for secret patterns.
func scanDiffForSecrets(ctx context.Context, dir, branch, baseBranch string) ([]SafetyIssue, error) {
	slog.Info("git diff for secrets", "branch", branch, "baseBranch", baseBranch)
	cmd := exec.CommandContext(ctx, "git", "diff", "origin/"+baseBranch+"..."+branch) //nolint:gosec // branch names are from internal git state.
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff for secret scan: %w: %s", err, stderr.String())
	}

	var issues []SafetyIssue
	seen := make(map[string]bool) // dedupe by file+kind
	var currentFile string

	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		// Track current file from diff headers.
		if after, ok := strings.CutPrefix(line, "+++ b/"); ok {
			currentFile = after
			continue
		}
		// Only scan added lines.
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		added := line[1:]
		for _, sp := range secretPatterns {
			if !sp.re.MatchString(added) {
				continue
			}
			key := currentFile + ":" + sp.desc
			if seen[key] {
				continue
			}
			seen[key] = true
			slog.Warn("secret pattern matched", "file", currentFile, "pattern", sp.desc, "line", added) //nolint:gosec // structured logging, no injection
			issues = append(issues, SafetyIssue{
				File:   currentFile,
				Kind:   "secret",
				Detail: fmt.Sprintf("possible %s detected", sp.desc),
			})
		}
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
