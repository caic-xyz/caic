# caic user installation

The installer downloads the latest checksummed caic and md releases, preserves
existing configuration, and starts caic's user service. Docker or Podman must be
installed and usable first. Run the same command to update; it takes no arguments.

```sh
curl -fsSL https://caic.xyz/install.sh | bash
```

Binaries are installed in `~/.local/bin`. Configuration is
`~/.config/caic/config.toml`, or `$XDG_CONFIG_HOME/caic/config.toml` when set.
See [config.toml](config.toml) for settings. caic loads the user's interactive
login shell environment at startup unless `core.shell_env = false`.

| Platform | User service                | Logs                                |
| -------- | --------------------------- | ----------------------------------- |
| Linux    | systemd `caic.service`      | `journalctl --user -u caic.service` |
| macOS    | launchd `com.fghbuild.caic` | `~/Library/Logs/caic/`              |

On Linux, caic starts with the systemd user manager. To start caic at boot
and keep it running after logout, enable lingering for the service account
with `loginctl enable-linger "$USER"`. If you previously enabled caic under
`graphical-session.target`, run `systemctl --user reenable caic.service`
after updating the unit to refresh its enablement links.

On Linux, tasks get KVM acceleration when the service user can write
`/dev/kvm`. The installer prints the missing step: join the device's group with
`sudo usermod -aG kvm "$USER"` and reboot, or enable virtualization in the
firmware settings. On Debian and Ubuntu, `sudo apt install cpu-checker && kvm-ok`
diagnoses a missing `/dev/kvm`.

For other installation workflows, [build caic from source](../README.md#from-source)
and install [md](https://github.com/caic-xyz/md) separately.

For standalone voice gateway installation and deployment, see the
[Go Mode deployment guide](https://github.com/gomodedev/gomode/tree/main/contrib).
