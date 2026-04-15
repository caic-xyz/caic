#!/bin/sh
# Install caic and md via: curl -fsSL https://caic.xyz/install.sh | bash
# Set NO_SERVICE=1 to skip service installation (systemd/launchd).
set -eu

INSTALL_DIR="${HOME}/.local/bin"
NO_SERVICE="${NO_SERVICE:-0}"

die() { echo "error: $*" >&2; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Linux)  echo linux ;;
        Darwin) echo darwin ;;
        *)      die "unsupported OS: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   echo amd64 ;;
        aarch64|arm64)  echo arm64 ;;
        *)              die "unsupported arch: $(uname -m)" ;;
    esac
}

# fetch URL to stdout. Uses GITHUB_TOKEN for auth if set.
fetch() {
    auth=""
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        auth="Authorization: Bearer ${GITHUB_TOKEN}"
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL ${auth:+-H "$auth"} "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- ${auth:+--header="$auth"} "$1"
    else
        die "curl or wget is required"
    fi
}

# fetch URL to file.
fetch_to() {
    auth=""
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        auth="Authorization: Bearer ${GITHUB_TOKEN}"
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL ${auth:+-H "$auth"} -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$2" ${auth:+--header="$auth"} "$1"
    else
        die "curl or wget is required"
    fi
}

latest_tag() {
    fetch "https://api.github.com/repos/$1/releases/latest" \
        | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}

verify_checksum() {
    archive_path="$1"
    archive_name="$2"
    checksums_path="$3"
    if command -v sha256sum >/dev/null 2>&1; then
        ( cd "$(dirname "$archive_path")" && grep " ${archive_name}$" "$checksums_path" | sha256sum -c - >/dev/null )
    elif command -v shasum >/dev/null 2>&1; then
        ( cd "$(dirname "$archive_path")" && grep " ${archive_name}$" "$checksums_path" | shasum -a 256 -c - >/dev/null )
    else
        printf '  warning: neither sha256sum nor shasum found, skipping checksum verification\n' >&2
        return 0
    fi
}

# download_and_extract <repo> <binary> <os> <arch> <tmpdir>
# Downloads, verifies, and extracts the archive into tmpdir.
download_and_extract() {
    repo="$1"
    binary="$2"
    os="$3"
    arch="$4"
    tmpdir="$5"

    tag="$(latest_tag "$repo")"
    [ -n "$tag" ] || die "could not determine latest version of $repo"
    version="${tag#v}"

    # macOS universal binary (darwin_all); Linux uses the specific arch.
    if [ "$os" = "darwin" ]; then
        archive="${binary}_${version}_${os}_all.tar.gz"
    else
        archive="${binary}_${version}_${os}_${arch}.tar.gz"
    fi
    url="https://github.com/${repo}/releases/download/${tag}/${archive}"

    printf 'Downloading %s %s\n' "$repo" "$tag"
    fetch_to "$url" "${tmpdir}/${archive}"

    checksum_url="https://github.com/${repo}/releases/download/${tag}/checksums.txt"
    if fetch_to "$checksum_url" "${tmpdir}/checksums.txt" 2>/dev/null; then
        verify_checksum "${tmpdir}/${archive}" "$archive" "${tmpdir}/checksums.txt" \
            || die "checksum verification failed for $archive"
    else
        printf '  warning: checksums.txt not found, skipping verification\n' >&2
    fi

    tar -xzf "${tmpdir}/${archive}" -C "$tmpdir"
}

install_binary() {
    binary="$1"
    tmpdir="$2"
    chmod +x "${tmpdir}/${binary}"
    mv "${tmpdir}/${binary}" "${INSTALL_DIR}/${binary}"
    printf '  installed %s → %s\n' "$binary" "${INSTALL_DIR}/${binary}"
}

# install_file <src> <dest> <mode>
# Copies src to dest if dest does not already exist.
install_file() {
    src="$1"
    dest="$2"
    mode="$3"
    if [ -f "$dest" ]; then
        printf 'Already exists: %s\n' "$dest"
        return
    fi
    mkdir -p "$(dirname "$dest")"
    cp "$src" "$dest"
    chmod "$mode" "$dest"
    printf '  installed %s\n' "$dest"
}

install_config() {
    tmpdir="$1"
    config_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/caic"
    src="${tmpdir}/contrib/config.toml"
    [ -f "$src" ] || return 0
    install_file "$src" "${config_dir}/config.toml" 0600
}

install_service_systemd() {
    tmpdir="$1"
    command -v systemctl >/dev/null 2>&1 || return 0
    src="${tmpdir}/contrib/caic.service"
    [ -f "$src" ] || return 0
    dest="${HOME}/.config/systemd/user/caic.service"
    install_file "$src" "$dest" 0644
    systemctl --user daemon-reload
    systemctl --user enable --now caic
}

install_service_launchd() {
    tmpdir="$1"
    src="${tmpdir}/contrib/com.caic.caic.plist"
    [ -f "$src" ] || return 0
    dest="${HOME}/Library/LaunchAgents/com.caic.caic.plist"
    if [ ! -f "$dest" ]; then
        mkdir -p "$(dirname "$dest")"
        sed "s|/Users/CHANGEME/.local/bin/caic|${INSTALL_DIR}/caic|" "$src" > "$dest"
        printf '  installed %s\n' "$dest"
    else
        printf 'Already exists: %s\n' "$dest"
    fi
    launchctl bootstrap "gui/$(id -u)" "$dest"
}

preflight() {
    if ! command -v docker >/dev/null 2>&1; then
        die "docker is not installed. Install Docker first: https://docs.docker.com/get-docker/"
    fi
    if ! docker info >/dev/null 2>&1; then
        die "docker is not accessible. Is the Docker daemon running? Is your user in the docker group?"
    fi
}

main() {
    preflight
    os="$(detect_os)"
    arch="$(detect_arch)"
    mkdir -p "$INSTALL_DIR"

    # Install caic (binaries + contrib files).
    caic_tmp="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$caic_tmp'" EXIT
    download_and_extract caic-xyz/caic caic "$os" "$arch" "$caic_tmp"
    install_binary caic "$caic_tmp"
    install_config "$caic_tmp"
    if [ "$NO_SERVICE" = "0" ]; then
        case "$os" in
            linux)  install_service_systemd "$caic_tmp" ;;
            darwin) install_service_launchd "$caic_tmp" ;;
        esac
    fi
    rm -rf "$caic_tmp"
    trap - EXIT

    # Install md (binary only).
    md_tmp="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$md_tmp'" EXIT
    download_and_extract caic-xyz/md md "$os" "$arch" "$md_tmp"
    install_binary md "$md_tmp"
    rm -rf "$md_tmp"
    trap - EXIT

    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            printf '\nAdd %s to your PATH:\n' "$INSTALL_DIR"
            # shellcheck disable=SC2016
            printf '  export PATH="%s:$PATH"\n' "$INSTALL_DIR"
            ;;
    esac

    print_url_err="$(mktemp)"
    if server_url="$("${INSTALL_DIR}/caic" -print-url 2>"$print_url_err")"; then
        printf '\nDone. The caic web server is accessible at %s\n' "$server_url"
    else
        printf '\nInstalled, but caic failed to resolve the server URL:\n'
        sed 's/^/  /' "$print_url_err"
        printf '\nCheck your configuration and try running: caic -print-url\n'
    fi
    rm -f "$print_url_err"
    printf 'See https://docs.caic.xyz/caic/configuration to get started.\n'
}

main
