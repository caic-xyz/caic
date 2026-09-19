// Git inspection reports status, comparison history, stats, and isolated file patches.

package mdruntime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const (
	gitLogMarker          = "caic-git-log"
	gitCommitMarker       = "caic-git-commit"
	gitComparisonMarker   = "# caic.branch.upstream "
	gitDivergenceMarker   = "# caic.branch.ab "
	gitOperationMarker    = "caic-git-operation"
	gitTotalStatMarker    = "caic-git-total-stat"
	gitWorktreeStatMarker = "caic-git-worktree-stat"
)

func gitStatusCommand(repo, defaultRemote, defaultBranch string) string {
	comparison := ""
	if defaultRemote != "" && defaultBranch != "" {
		comparison = defaultRemote + "/" + defaultBranch
	}
	return "cd " + shellQuote(repo) + ` && export GIT_OPTIONAL_LOCKS=0 LC_ALL=C && ` +
		`git status --porcelain=v2 --branch -z --untracked-files=all && ` +
		`upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null || true) && ` +
		`comparison=` + shellQuote(comparison) + ` && ` +
		`if ! git rev-parse --verify --quiet "$comparison^{commit}" >/dev/null; then comparison=$upstream; fi && ` +
		`if [ -n "$comparison" ]; then ` +
		`divergence=$(git rev-list --left-right --count "$comparison...HEAD") && ` +
		`printf '` + gitComparisonMarker + `%s\0` + gitDivergenceMarker + `%s\0' "$comparison" "$divergence"; ` +
		`fi && git_dir=$(git rev-parse --git-dir) && operation= && ` +
		`if [ -d "$git_dir/rebase-merge" ] || [ -d "$git_dir/rebase-apply" ]; then operation=rebase; ` +
		`elif git rev-parse --verify --quiet MERGE_HEAD >/dev/null; then operation=merge; ` +
		`elif git rev-parse --verify --quiet CHERRY_PICK_HEAD >/dev/null; then operation=cherry-pick; ` +
		`elif git rev-parse --verify --quiet REVERT_HEAD >/dev/null; then operation=revert; ` +
		`elif [ -f "$git_dir/BISECT_LOG" ]; then operation=bisect; fi && ` +
		`if [ -n "$operation" ]; then printf '` + gitOperationMarker + `\0%s\0' "$operation"; fi && ` +
		`printf '` + gitTotalStatMarker + `\0' && ` +
		`if [ -n "$comparison" ]; then ` + alternateIndexDiffCommand(`git diff "$comparison" --numstat --stat -z -- .`) + `; fi && ` +
		`printf '\0` + gitWorktreeStatMarker + `\0' && ` +
		alternateIndexDiffCommand("git diff HEAD --numstat --stat -z -- .") + ` && ` +
		`printf '\0` + gitLogMarker + `\0' && ` +
		`if [ -n "$comparison" ]; then git log --date-order --decorate=short --no-color ` +
		`--format='%x00` + gitCommitMarker + `%x00%H%x00%as%x00%D%x00%s%x00' --numstat --stat -z "$comparison..HEAD"; fi`
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func gitFileDiffCommand(repo, commit, path, originalPath string) (string, error) {
	if path == "" {
		return "", errors.New("git file diff path is required")
	}
	if commit != "" && !isGitObjectID(commit) {
		return "", errors.New("git file diff commit must be a full object ID")
	}
	commands := []string{
		"cd " + shellQuote(repo),
		"export GIT_OPTIONAL_LOCKS=0 LC_ALL=C",
	}
	if commit == "" {
		pathspec := shellQuote(path)
		if originalPath != "" && originalPath != path {
			pathspec = shellQuote(originalPath) + " " + pathspec
		}
		commands = append(commands, alternateIndexDiffCommand(gitPatchCommand("diff")+" HEAD -- "+pathspec))
	} else {
		commands = append(commands, gitPatchCommand("show")+" --format= --diff-merges=first-parent --follow "+shellQuote(commit)+" -- "+shellQuote(path))
	}
	return strings.Join(commands, " && "), nil
}

func gitCommitDiffStatCommand(repo, from, to string) (string, error) {
	if !isGitObjectID(from) || !isGitObjectID(to) {
		return "", errors.New("git commit diff stat requires full object IDs")
	}
	return strings.Join([]string{
		"cd " + shellQuote(repo),
		"export GIT_OPTIONAL_LOCKS=0 LC_ALL=C",
		"git diff --numstat --stat --find-renames=50% " + shellQuote(from) + " " + shellQuote(to) + " --",
	}, " && "), nil
}

func gitPatchCommand(subcommand string) string {
	return "git -c core.quotePath=true -c diff.mnemonicPrefix=false -c diff.noprefix=false " + subcommand +
		" --patch --diff-algorithm=myers --no-indent-heuristic --unified=3 --inter-hunk-context=0" +
		" --src-prefix=a/ --dst-prefix=b/ --output-indicator-new=+ --output-indicator-old=-" +
		" --output-indicator-context=" + shellQuote(" ") +
		" --no-color --no-ext-diff --no-relative --no-textconv --find-renames=50% --full-index" +
		" --ita-visible-in-index" +
		" --ignore-submodules=none --submodule=short"
}

func isGitObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if ('0' > c || c > '9') && ('a' > c || c > 'f') && ('A' > c || c > 'F') {
			return false
		}
	}
	return true
}

func parseGitStatus(out string) (runtime.RepositoryStatus, error) {
	var status runtime.RepositoryStatus
	records := strings.Split(out, "\x00")
	logStart := -1
	for i := 0; i < len(records); {
		record := records[i]
		if record == gitLogMarker {
			logStart = i + 1
			break
		}
		if record == gitWorktreeStatMarker {
			consumed, err := parseGitWorktreeStats(&status, records[i+1:])
			if err != nil {
				return runtime.RepositoryStatus{}, err
			}
			i += consumed + 1
			continue
		}
		if record == gitTotalStatMarker {
			stats, consumed, err := parseGitNumstats(records[i+1:], gitWorktreeStatMarker)
			if err != nil {
				return runtime.RepositoryStatus{}, err
			}
			status.DiffStat = stats
			i += consumed + 1
			continue
		}
		consumed, err := parseGitStatusRecord(&status, records[i:])
		if err != nil {
			return runtime.RepositoryStatus{}, err
		}
		i += consumed
	}
	if logStart == -1 {
		return runtime.RepositoryStatus{}, errors.New("git status output is missing log marker")
	}
	for i := logStart; i < len(records); {
		if records[i] == "" {
			i++
			continue
		}
		if records[i] != gitCommitMarker {
			return runtime.RepositoryStatus{}, fmt.Errorf("unknown git log record %q", records[i])
		}
		if i+5 >= len(records) {
			return runtime.RepositoryStatus{}, errors.New("incomplete git commit record")
		}
		commit := runtime.GitCommit{
			SHA:          records[i+1],
			AuthoredDate: records[i+2],
			Decorations:  records[i+3],
			Subject:      records[i+4],
		}
		i += 5
		for i < len(records) && records[i] != gitCommitMarker {
			record := strings.TrimPrefix(records[i], "\n")
			if record == "" {
				i++
				continue
			}
			file, consumed, err := parseGitNumstatRecord(records[i:])
			if err != nil {
				// git log --numstat --stat appends a --stat block after each
				// commit's numstat records; attach its binary sizes by position.
				if !attachGitStatSizes(commit.Stat, records[i]) {
					return runtime.RepositoryStatus{}, err
				}
				i++
				continue
			}
			commit.Stat = append(commit.Stat, file)
			i += consumed
		}
		status.Commits = append(status.Commits, commit)
	}
	return status, nil
}

func alternateIndexDiffCommand(diffCommand string) string {
	return strings.Join([]string{
		`index_path=$(git rev-parse --git-path index)`,
		`tmp_index=$(mktemp)`,
		`untracked_paths=$(mktemp)`,
		`cp -p "$index_path" "$tmp_index"`,
		`trap 'rm -f "$tmp_index" "$untracked_paths"' EXIT`,
		`git ls-files -z --others --exclude-standard -- . > "$untracked_paths"`,
		`while IFS= read -r -d '' path; do GIT_INDEX_FILE="$tmp_index" git add -N -- "$path" || exit $?; done < "$untracked_paths"`,
		`GIT_INDEX_FILE="$tmp_index" ` + diffCommand,
	}, " && ")
}

func parseGitWorktreeStats(status *runtime.RepositoryStatus, records []string) (int, error) {
	stats, consumed, err := parseGitNumstats(records, gitLogMarker)
	if err != nil {
		return 0, err
	}
	for _, stat := range stats {
		for j := range status.Uncommitted {
			if status.Uncommitted[j].Path != stat.Path {
				continue
			}
			status.Uncommitted[j].LinesAdded = stat.LinesAdded
			status.Uncommitted[j].LinesDeleted = stat.LinesDeleted
			status.Uncommitted[j].Binary = stat.Binary
			status.Uncommitted[j].OldSize = stat.OldSize
			status.Uncommitted[j].NewSize = stat.NewSize
			break
		}
	}
	return consumed, nil
}

func parseGitNumstats(records []string, endMarker string) ([]runtime.GitFileStat, int, error) {
	stats := make([]runtime.GitFileStat, 0, len(records))
	for i := 0; i < len(records); {
		if records[i] == endMarker {
			return stats, i, nil
		}
		if records[i] == "" {
			i++
			continue
		}
		stat, consumed, err := parseGitNumstatRecord(records[i:])
		if err != nil {
			// git diff --numstat --stat appends a --stat block after the numstat
			// records; consume it and attach binary sizes by position.
			if !attachGitStatSizes(stats, records[i]) {
				return nil, 0, err
			}
			i++
			continue
		}
		stats = append(stats, stat)
		i += consumed
	}
	return stats, len(records), nil
}

// attachGitStatSizes parses one git diff --stat block and attaches the binary
// pre-image and post-image sizes to stats. git lists files in the same order for
// --stat and --numstat, so sizes are matched by position. It reports whether
// block was a --stat block rather than a malformed numstat record.
func attachGitStatSizes(stats []runtime.GitFileStat, block string) bool {
	if !strings.Contains(block, " | ") {
		return false
	}
	index := 0
	for line := range strings.SplitSeq(block, "\n") {
		if !strings.Contains(line, " | ") {
			continue
		}
		if index >= len(stats) {
			break
		}
		if oldSize, newSize, ok := parseGitStatBinarySizes(line); ok {
			stats[index].Binary = true
			stats[index].OldSize = oldSize
			stats[index].NewSize = newSize
		}
		index++
	}
	return true
}

// parseGitStatBinarySizes extracts the "Bin <old> -> <new> bytes" sizes from one
// git diff --stat row, reporting whether the row describes a binary file.
func parseGitStatBinarySizes(line string) (oldSize, newSize int64, ok bool) {
	const marker = "| Bin "
	_, after, ok := strings.Cut(line, marker)
	if !ok {
		return 0, 0, false
	}
	rest := strings.TrimSuffix(after, " bytes")
	oldStr, newStr, found := strings.Cut(rest, " -> ")
	if !found {
		return 0, 0, false
	}
	oldSize, oldErr := strconv.ParseInt(oldStr, 10, 64)
	newSize, newErr := strconv.ParseInt(newStr, 10, 64)
	if oldErr != nil || newErr != nil {
		return 0, 0, false
	}
	return oldSize, newSize, true
}

func parseGitNumstatRecord(records []string) (runtime.GitFileStat, int, error) {
	record := strings.TrimPrefix(records[0], "\n")
	fields := strings.SplitN(record, "\t", 3)
	if len(fields) != 3 {
		return runtime.GitFileStat{}, 0, fmt.Errorf("parse git numstat record %q", record)
	}
	path := fields[2]
	consumed := 1
	if path == "" {
		if len(records) < 3 {
			return runtime.GitFileStat{}, 0, fmt.Errorf("parse renamed git numstat record %q", record)
		}
		path = records[2]
		consumed = 3
	}
	if fields[0] == "-" && fields[1] == "-" {
		return runtime.GitFileStat{Path: path, Binary: true}, consumed, nil
	}
	added, err := strconv.Atoi(fields[0])
	if err != nil {
		return runtime.GitFileStat{}, 0, fmt.Errorf("parse git additions %q: %w", fields[0], err)
	}
	deleted, err := strconv.Atoi(fields[1])
	if err != nil {
		return runtime.GitFileStat{}, 0, fmt.Errorf("parse git deletions %q: %w", fields[1], err)
	}
	return runtime.GitFileStat{Path: path, LinesAdded: added, LinesDeleted: deleted}, consumed, nil
}

func parseGitStatusRecord(status *runtime.RepositoryStatus, records []string) (int, error) {
	record := records[0]
	switch {
	case strings.HasPrefix(record, "# branch.head "):
		status.Branch = strings.TrimPrefix(record, "# branch.head ")
	case strings.HasPrefix(record, "# branch.upstream "):
		status.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
	case strings.HasPrefix(record, "# branch.ab "):
		if _, err := fmt.Sscanf(strings.TrimPrefix(record, "# branch.ab "), "+%d -%d", &status.Ahead, &status.Behind); err != nil {
			return 0, fmt.Errorf("parse branch divergence %q: %w", record, err)
		}
	case strings.HasPrefix(record, gitComparisonMarker):
		status.Upstream = strings.TrimPrefix(record, gitComparisonMarker)
	case strings.HasPrefix(record, gitDivergenceMarker):
		if _, err := fmt.Sscanf(strings.TrimPrefix(record, gitDivergenceMarker), "%d %d", &status.Behind, &status.Ahead); err != nil {
			return 0, fmt.Errorf("parse comparison divergence %q: %w", record, err)
		}
	case record == gitOperationMarker:
		if len(records) < 2 {
			return 0, errors.New("git status output is missing operation")
		}
		operation := runtime.RepositoryOperation(records[1])
		switch operation {
		case runtime.RepositoryOperationRebase, runtime.RepositoryOperationMerge, runtime.RepositoryOperationCherryPick, runtime.RepositoryOperationRevert, runtime.RepositoryOperationBisect:
			status.Operation = operation
		default:
			return 0, fmt.Errorf("unknown git operation %q", operation)
		}
		return 2, nil
	case strings.HasPrefix(record, "1 "):
		fields := strings.SplitN(record, " ", 9)
		if len(fields) != 9 {
			return 0, fmt.Errorf("parse ordinary git status record %q", record)
		}
		status.Uncommitted = append(status.Uncommitted, fileStatus(fields[1], fields[8], ""))
	case strings.HasPrefix(record, "2 "):
		fields := strings.SplitN(record, " ", 10)
		if len(fields) != 10 || len(records) < 2 {
			return 0, fmt.Errorf("parse renamed git status record %q", record)
		}
		status.Uncommitted = append(status.Uncommitted, fileStatus(fields[1], fields[9], records[1]))
		return 2, nil
	case strings.HasPrefix(record, "u "):
		fields := strings.SplitN(record, " ", 11)
		if len(fields) != 11 {
			return 0, fmt.Errorf("parse unmerged git status record %q", record)
		}
		status.Uncommitted = append(status.Uncommitted, fileStatus(fields[1], fields[10], ""))
	case strings.HasPrefix(record, "? "):
		status.Uncommitted = append(status.Uncommitted, fileStatus("??", strings.TrimPrefix(record, "? "), ""))
	case record == "", strings.HasPrefix(record, "# branch.oid "):
	default:
		return 0, fmt.Errorf("unknown git status record %q", record)
	}
	return 1, nil
}

func fileStatus(code, path, originalPath string) runtime.GitFileStatus {
	if len(code) != 2 {
		return runtime.GitFileStatus{Path: path, OriginalPath: originalPath}
	}
	return runtime.GitFileStatus{
		Path:           path,
		OriginalPath:   originalPath,
		IndexStatus:    statusCode(code[0]),
		WorktreeStatus: statusCode(code[1]),
	}
}

func statusCode(code byte) string {
	if code == '.' || code == ' ' {
		return ""
	}
	return string(code)
}
