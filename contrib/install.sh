#!/bin/bash
# Install the latest checksummed caic and md releases and start caic's user service.

set -euo pipefail
umask 077

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}
(($# == 0)) || fail 'This installer takes no arguments.'
case "$(uname -s)" in
Linux) platform=linux ;;
Darwin) platform=darwin ;;
*) fail 'This installer supports Linux and macOS.' ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail 'Supported architectures are amd64 and arm64.' ;;
esac
config_home="${XDG_CONFIG_HOME:-${HOME:?HOME must be set}/.config}"
for path in "$HOME" "$config_home"; do
  [[ "$path" == /* ]] || fail 'HOME and XDG_CONFIG_HOME must be absolute paths.'
  if printf '%s' "$path" | LC_ALL=C grep -q '[[:cntrl:]]'; then fail 'User paths cannot contain control characters.'; fi
done
install_dir="$HOME/.local/bin"
config_path="$config_home/caic/config.toml"
if [[ "$platform" == linux ]]; then
  service_path="$config_home/systemd/user/caic.service"
  command -v systemctl >/dev/null || fail 'systemctl is required.'
  systemctl --user show-environment >/dev/null
else
  service_path="$HOME/Library/LaunchAgents/com.fghbuild.caic.plist"
  logs="$HOME/Library/Logs/caic"
  command -v launchctl >/dev/null || fail 'launchctl is required.'
  command -v plutil >/dev/null || fail 'plutil is required.'
fi
for tool in curl tar install; do command -v "$tool" >/dev/null || fail "$tool is required."; done
if command -v sha256sum >/dev/null; then
  hash_command=(sha256sum)
elif command -v shasum >/dev/null; then
  hash_command=(shasum -a 256)
else
  fail 'sha256sum or shasum is required.'
fi
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  :
elif command -v podman >/dev/null && podman info >/dev/null 2>&1; then
  :
else
  fail 'A usable Docker or Podman installation is required.'
fi
destinations=("$install_dir/caic" "$install_dir/md" "$service_path")
if [[ "$platform" == darwin ]]; then destinations+=("$logs/stdout.log" "$logs/stderr.log"); fi
for path in "$config_path" "${destinations[@]}"; do
  [[ ! -e "$path" || -f "$path" ]] || fail "Expected a file: $path"
done
for path in "${destinations[@]}"; do
  for settings in "$config_path" "$config_home/caic/settings.json" "$HOME/.config/systemd/user/caic.env"; do
    [[ ! "$path" -ef "$settings" ]] || fail 'An installation destination aliases configuration or credentials.'
  done
done
service_command_path="$install_dir:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
work_dir="$(mktemp -d)"
readonly work_dir
pending=''
completed=()
cleanup() {
  status=$?
  trap - EXIT
  rm -rf -- "$work_dir"
  if [[ -n "$pending" ]]; then rm -f -- "$pending"; fi
  if ((status != 0 && ${#completed[@]} != 0)); then
    printf 'Installation failed after installing:\n' >&2
    printf '  %s\n' "${completed[@]}" >&2
    printf 'Correct the error and rerun.\n' >&2
  fi
  exit "$status"
}
trap cleanup EXIT

prepare_release() {
  local repo="$1" binary="$2" resolved tag version asset base expected actual hash line matches member count
  local stage="$work_dir/$binary"
  local members=("$binary")
  mkdir -- "$stage"
  resolved="$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --head --output /dev/null --write-out '%{url_effective}' "https://github.com/$repo/releases/latest")"
  tag="${resolved##*/}"
  local tag_pattern='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
  [[ "$tag" =~ $tag_pattern ]] || fail "Could not resolve the latest stable release of $repo."
  version="${tag#v}"
  if [[ "$platform" == darwin ]]; then asset="${binary}_${version}_darwin_all.tar.gz"; else asset="${binary}_${version}_linux_${arch}.tar.gz"; fi
  base="https://github.com/$repo/releases/download/$tag"
  printf 'Downloading %s %s\n' "$binary" "$tag"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' "$base/$asset" --output "$stage/archive.tar.gz"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' "$base/checksums.txt" --output "$stage/checksums.txt"
  expected=''
  matches=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    hash="${line:0:64}"
    if [[ "$line" == "$hash  $asset" && "$hash" =~ ^[[:xdigit:]]{64}$ ]]; then
      expected="$hash"
      matches=$((matches + 1))
    fi
  done <"$stage/checksums.txt"
  ((matches == 1)) || fail "Expected exactly one SHA-256 entry for $asset."
  expected="$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')"
  actual="$("${hash_command[@]}" <"$stage/archive.tar.gz")"
  [[ "${actual%% *}" == "$expected" ]] || fail "Archive SHA-256 checksum mismatch: $asset."
  if [[ "$binary" == caic ]]; then
    members+=(contrib/config.toml)
    if [[ "$platform" == linux ]]; then members+=(contrib/caic.service); else members+=(contrib/com.fghbuild.caic.plist); fi
  fi
  tar -tzf "$stage/archive.tar.gz" >"$stage/members"
  for member in "${members[@]}"; do
    count=0
    while IFS= read -r line; do if [[ "$line" == "$member" ]]; then count=$((count + 1)); fi; done <"$stage/members"
    ((count == 1)) || fail "Archive must contain exactly one $member."
  done
  # Extract only the required regular files, never archive links or unrelated paths.
  tar -tvzf "$stage/archive.tar.gz" "${members[@]}" >"$stage/types"
  while IFS= read -r line; do [[ "$line" == -* ]] || fail 'Required archive members must be regular files.'; done <"$stage/types"
  mkdir -- "$stage/files"
  tar -xzf "$stage/archive.tar.gz" -C "$stage/files" "${members[@]}"
  [[ -s "$stage/files/$binary" && -x "$stage/files/$binary" ]] || fail "Archive binary must be nonempty and executable: $binary."
}

prepare_release caic-xyz/caic caic
prepare_release caic-xyz/md md
if [[ "$platform" == linux ]]; then
  config_environment="$(printf '%s' "$config_home" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')"
  command_environment="$(printf '%s' "$service_command_path" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')"
  while IFS= read -r line || [[ -n "$line" ]]; do
    printf '%s\n' "$line"
    if [[ "$line" == '[Service]' ]]; then
      printf 'Environment="XDG_CONFIG_HOME=%s"\nEnvironment="PATH=%s"\n' "$config_environment" "$command_environment"
    fi
  done <"$work_dir/caic/files/contrib/caic.service" >"$work_dir/service"
else
  cp -- "$work_dir/caic/files/contrib/com.fghbuild.caic.plist" "$work_dir/service"
  plutil -replace ProgramArguments.0 -string "$install_dir/caic" -- "$work_dir/service"
  plutil -replace StandardOutPath -string "$logs/stdout.log" -- "$work_dir/service"
  plutil -replace StandardErrorPath -string "$logs/stderr.log" -- "$work_dir/service"
  plutil -insert EnvironmentVariables -xml '<dict/>' -- "$work_dir/service"
  plutil -insert EnvironmentVariables.XDG_CONFIG_HOME -string "$config_home" -- "$work_dir/service"
  plutil -insert EnvironmentVariables.PATH -string "$service_command_path" -- "$work_dir/service"
  plutil -lint -s -- "$work_dir/service"
fi
mkdir -p -- "$install_dir" "$(dirname -- "$config_path")" "$(dirname -- "$service_path")"
for binary in caic md; do
  pending="$(mktemp "$install_dir/.$binary.XXXXXX")"
  install -m 755 "$work_dir/$binary/files/$binary" "$pending"
  mv -f -- "$pending" "$install_dir/$binary"
  pending=''
  completed+=("$binary")
  printf 'Installed: %s\n' "$install_dir/$binary"
done
if [[ ! -e "$config_path" ]]; then
  (
    set -o noclobber
    cat "$work_dir/caic/files/contrib/config.toml" >"$config_path"
  )
  completed+=("configuration")
fi
pending="$(mktemp "$(dirname -- "$service_path")/.caic-service.XXXXXX")"
install -m 644 "$work_dir/service" "$pending"
mv -f -- "$pending" "$service_path"
pending=''
completed+=("user service")
if [[ "$platform" == linux ]]; then
  systemctl --user daemon-reload
  systemctl --user enable caic.service
  systemctl --user restart caic.service
else
  mkdir -p -- "$logs"
  domain="gui/$(id -u)"
  if launchctl print "$domain/com.fghbuild.caic" >/dev/null 2>&1; then launchctl bootout "$domain/com.fghbuild.caic"; fi
  launchctl bootstrap "$domain" "$service_path"
fi
printf 'Configuration: %s\nSee https://docs.caic.xyz/caic/configuration to get started.\n' "$config_path"
case ":$PATH:" in
*":$install_dir:"*) ;;
*) printf 'Add %s to PATH to use caic and md from your terminal.\n' "$install_dir" ;;
esac
