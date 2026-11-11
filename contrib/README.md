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

For other installation workflows, [build caic from source](../README.md#from-source)
and install [md](https://github.com/caic-xyz/md) separately.

For standalone voice gateway installation and deployment, see the
[Go Mode deployment guide](https://github.com/maruel/gomode/tree/main/contrib).
