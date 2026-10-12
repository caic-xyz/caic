# contrib

Platform service files and default configuration for running caic as a daemon.

## Files

- `config.toml` — Default configuration; copy to `~/.config/caic/config.toml`
  and edit.
- `voice-gateway-config.toml` — Standalone voice gateway configuration; copy to
  `~/.config/voice-gateway/config.toml` and edit.
- `com.caic.caic.plist` — macOS launchd user agent with system-path defaults;
  the installer overrides them with per-user paths.
- `caic.service` — Linux systemd user service; starts Podman's packaged user
  socket when available.
- `install.sh` — Installer script served at `https://caic.xyz/install.sh`.

## macOS (launchd)

```bash
# Install (or replace an existing plist to update its paths)
plist="$HOME/Library/LaunchAgents/com.caic.caic.plist"
logs="$HOME/Library/Logs/caic"
mkdir -p "$HOME/Library/LaunchAgents"
(umask 077 && mkdir -p "$logs")
cp contrib/com.caic.caic.plist "$plist"
/usr/bin/plutil -replace ProgramArguments.0 -string "$HOME/.local/bin/caic" -- "$plist"
/usr/bin/plutil -replace StandardOutPath -string "$logs/stdout.log" -- "$plist"
/usr/bin/plutil -replace StandardErrorPath -string "$logs/stderr.log" -- "$plist"
/usr/bin/plutil -lint -- "$plist"
# caic loads the user's interactive login shell environment at startup.
# Export tool paths and SSH_AUTH_SOCK in the shell's startup configuration.

# Enable
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Disable
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Restart (after editing the plist)
# launchctl has no "reload" — you must bootout then bootstrap.
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Logs
tail -f "$HOME/Library/Logs/caic/stdout.log" "$HOME/Library/Logs/caic/stderr.log"
```

The unmodified template launches `/usr/local/bin/caic` and writes logs to
`/var/log/caic/stdout.log` and `/var/log/caic/stderr.log`. To use these defaults,
install the binary there and provision the log directory and writable log files
for the account running caic. System paths do not turn the user agent into a
system daemon.

The installer configures a new plist with the per-user paths above. It keeps an
existing plist unchanged. To update an existing service, run the install commands
above, then bootout and bootstrap it. launchd starts caic directly, without a shell
wrapper. It creates missing log files; the log directory must exist first.
Log rotation needs separate configuration.

## Linux (systemd)

```bash
# Install
mkdir -p ~/.config/systemd/user
cp contrib/caic.service ~/.config/systemd/user/

# Enable and start. With Podman installed, podman.socket starts its API service
# on demand. The socket is optional for Docker-only setups.
systemctl --user daemon-reload
systemctl --user enable --now caic

# If using Podman, confirm its socket is active.
systemctl --user status podman.socket

# Restart (after editing the unit file)
systemctl --user daemon-reload
systemctl --user restart caic

# Logs
journalctl --user -u caic -f
```

## Shell environment

On macOS and Linux, caic runs the user's interactive login shell once at startup,
without a terminal. It imports exported variables before finding local tools.
`SHELL` selects the shell when present; otherwise caic queries the account database.
The snapshot uses `/usr/bin/env -0`, included in GNU/Linux and modern macOS.
A dedicated pipe keeps shell startup messages out of the snapshot.
Shell exports replace inherited values, including `PATH` and `SSH_AUTH_SOCK`.
Process/session bookkeeping stays unchanged. Explicit `[core.env]` values retain
precedence for caic's configured credentials.

The probe waits at most six seconds. On failure, caic logs a warning and keeps its
inherited environment. It does not log shell output or exported values. Config
location selection, `--version`, and `--print-url` do not load the shell.

To use only the service or terminal environment, set this in `config.toml`:

```toml
[core]
shell_env = false
```

Shell loading finds tools such as Git and npm when startup scripts export their
paths. It does not import aliases or functions, refresh version-manager settings
per repository, unlock SSH keys, or repair a dead agent socket. Commands inside
containers still use the container environment. Keep shell startup scripts
nonblocking when no terminal is present.

Linux no longer requires a GNOME keyring socket. Configure your shell or service
with the socket of your existing SSH agent. With `shell_env = false`, service
overrides can go in `~/.config/systemd/user/caic.env`.

## Configuration

```bash
mkdir -p ~/.config/caic
cp contrib/config.toml ~/.config/caic/config.toml
# Edit as needed; see config.toml for documentation.
```

Standalone voice gateway configuration:

```bash
mkdir -p ~/.config/voice-gateway
cp contrib/voice-gateway-config.toml ~/.config/voice-gateway/config.toml
# Edit as needed; see voice-gateway-config.toml for documentation.
```
