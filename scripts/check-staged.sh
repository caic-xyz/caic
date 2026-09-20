#!/bin/bash
# Validates the staged snapshot with focused repository checks.

set -euo pipefail

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(CDPATH='' cd -- "$script_dir/.." && pwd)"
readonly repo_root

cd -- "$repo_root"

# Read the complete staged snapshot first so each project-specific trigger is
# based on what will actually be committed.
staged_changes=()
frontend_source_changed=false

while IFS= read -r -d '' file; do
  staged_changes+=("$file")
  case "$file" in
  frontend/src/*.css | frontend/src/*.html | frontend/src/*.json | frontend/src/*.svg | frontend/src/*.ts | frontend/src/*.tsx)
    frontend_source_changed=true
    ;;
  esac
done < <(git diff --cached --name-only -z)

if ((${#staged_changes[@]} == 0)); then
  exit 0
fi

# Focused checks read the worktree, so require it to be exactly the staged
# snapshot before running them.
if ! git diff --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  printf '%s\n' 'Stage or remove all changes before committing; checks run against the staged snapshot.' >&2
  exit 1
fi

# Classify added, copied, modified, and renamed files for focused checks. Only a file
# named AGENTS.md (or its CLAUDE.md symlink, which the symlink check skips) is exempt
# from formatting: a doc whose name merely ends in AGENTS.md is a normal source file.
format_files=()
eslint_files=()
go_files=()
python_files=()
style_files=()

while IFS= read -r -d '' file; do
  if [[ ! -L "$file" ]]; then
    case "$file" in
    AGENTS.md | */AGENTS.md)
      ;;
    *.css | *.html | *.json | *.md | *.mjs | *.ts | *.tsx | *.yaml | *.yml)
      format_files+=("$file")
      ;;
    esac
  fi
  # These trees are globally ignored by eslint.config.js; keep this list in sync with that
  # config, which also covers the generated SDK TypeScript that .prettierignore skips. Handing
  # an ignored path to eslint only adds a "File ignored because of a matching ignore pattern"
  # warning, so leave such files out of the list.
  case "$file" in
  backend/* | sdk/* | frontend/dist/* | frontend/public/* | android/*)
    ;;
  *.js | *.mjs | *.ts | *.tsx)
    eslint_files+=("$file")
    ;;
  *.go)
    go_files+=("$file")
    ;;
  *.py)
    python_files+=("$file")
    ;;
  frontend/src/*.css)
    style_files+=("$file")
    ;;
  esac
done < <(git diff --cached --name-only --diff-filter=ACMR -z)

# Checks only read the staged snapshot, so run their independent work in
# parallel while retaining exact argument arrays for paths with spaces.
check_names=()
check_pids=()

run_check() {
  local name=$1
  shift
  "$@" &
  check_names+=("$name")
  check_pids+=("$!")
}

wait_for_checks() {
  local rc=0
  local i
  for i in "${!check_pids[@]}"; do
    if ! wait "${check_pids[$i]}"; then
      printf 'Staged check failed: %s\n' "${check_names[$i]}" >&2
      rc=1
    fi
  done
  return "$rc"
}

check_gofmt() {
  local output
  output=$(gofmt -d "$@")
  if [[ -n "$output" ]]; then
    printf '%s\n' "$output"
    return 1
  fi
}

if ((${#format_files[@]} > 0)); then
  run_check format pnpm exec prettier --check --log-level warn -- "${format_files[@]}"
fi

if ((${#eslint_files[@]} > 0)); then
  # --no-warn-ignored keeps a future change to the ignore list from turning a staged
  # ignored file back into a warning without failing the push.
  run_check eslint pnpm exec eslint --no-warn-ignored -- "${eslint_files[@]}"
fi

if ((${#go_files[@]} > 0)); then
  run_check gofmt check_gofmt "${go_files[@]}"
fi

if ((${#python_files[@]} > 0)); then
  command -v ruff >/dev/null || {
    printf '%s\n' 'ruff is required to validate staged Python files.' >&2
    exit 1
  }
  run_check python-lint ruff check --quiet -- "${python_files[@]}"
  run_check python-format ruff format --check --quiet -- "${python_files[@]}"
fi

if ((${#style_files[@]} > 0)); then
  run_check style pnpm --silent lint:style:files -- "${style_files[@]}"
fi

if "$frontend_source_changed"; then
  run_check frontend-styles node scripts/lint_frontend_styles.mjs
fi

run_check binaries python3 scripts/lint_binaries.py
run_check agents python3 scripts/update_agents_file_index.py --check
run_check architecture python3 scripts/update_backend_architecture.py --check
wait_for_checks
