# contrib

Platform service files and default configuration for running caic as a daemon.

## Files

- `config.toml` — Default configuration; copy to `~/.config/caic/config.toml`
  and edit.
- `voice-gateway-config.toml` — Standalone voice gateway configuration; copy to
  `~/.config/voice-gateway/config.toml` and edit.
- `com.caic.caic.plist` — macOS launchd user agent.
- `caic.service` — Linux systemd user service; starts Podman's packaged user
  socket when available.
- `install.sh` — Installer script served at `https://caic.xyz/install.sh`.

## macOS (launchd)

```bash
# Install
cp contrib/com.caic.caic.plist ~/Library/LaunchAgents/
# The agent runs caic through the account's login shell, loading its login profile.
# Export service variables from ~/.zprofile (zsh) or ~/.bash_profile (Bash).

# Enable
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Disable
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Restart (after editing the plist)
# launchctl has no "reload" — you must bootout then bootstrap.
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.caic.caic.plist

# Logs
tail -f /tmp/caic.log
```

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
