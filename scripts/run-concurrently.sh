#!/bin/bash
# Runs independent commands concurrently, or sequentially in serial mode.
# Serial mode: pass --serial as the first argument or set RUN_CONCURRENTLY_SERIAL=1.

set -euo pipefail

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repository_dir="$(CDPATH='' cd -- "$script_dir/.." && pwd)"
readonly repository_dir

serial=false
if [[ "${1:-}" == "--serial" ]]; then
  serial=true
  shift
elif [[ "${RUN_CONCURRENTLY_SERIAL:-0}" != "0" ]]; then
  serial=true
fi

if (($# < 2)); then
  printf 'Usage: %s [--serial] names command [command ...]\n' "$0" >&2
  exit 2
fi
concurrently_path="$repository_dir/node_modules/.bin/concurrently"
if [[ ! -x "$concurrently_path" ]]; then
  printf '%s\n' 'Run pnpm install before running independent commands.' >&2
  exit 1
fi
readonly concurrently_path

names="$1"
readonly names
shift
arguments=(--names "$names")
# To diagnose slow lanes, add --timings here: concurrently then logs when each
# command starts and stops and ends with a per-command duration table. It is
# off by default because it costs the color output and adds noise.
if "$serial"; then
  arguments+=(--max-processes 1)
fi

cd -- "$repository_dir"
exec "$concurrently_path" "${arguments[@]}" "$@"
