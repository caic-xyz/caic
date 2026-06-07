# Halo Device Support

Supporting the [Brilliant Halo](https://docs.brilliant.xyz/halo/halo/) smart glasses
over Bluetooth LE from both Android apps:

- `:caic` (`com.fghbuild.caic`) owns caic task peripheral behavior.
- `:gomode` (`com.fghbuild.gomode`) owns shell-level Halo device management and
  must stay independent of caic domain APIs.
- Shared BLE transport and typed messaging code belongs in `:halo-sdk`.

## Architecture Boundary

```
Android apps
  ├── :caic
  │   └── com.fghbuild.caic.halo.HaloService       task state -> Halo display, clicks -> actions
  ├── :gomode
  │   └── com.fghbuild.gomode.halo.HaloController  shell-owned scan/connect/disconnect
  └── :halo-sdk
      ├── com.caic.halo.msg.HalosideApp            device-side app lifecycle
      └── com.caic.halo.ble                        BLE transport
```

Protocol details live in [sdk/halo/PROTOCOL.md](../../sdk/halo/PROTOCOL.md).
SDK development notes live in [sdk/halo/AGENTS.md](../../sdk/halo/AGENTS.md).

## Remaining Work

### caic Peripheral

- Auto-reconnect: observe `SettingsRepository.haloAddress` and reconnect on app
  start when `haloAutoConnect` is enabled.
- Task state display: send `TxSprite` state badges instead of text-only
  `TxPlainText` status lines.
- Notifications: suppress `TaskNotifier` attention notifications while Halo is
  connected.
- Double click: read the latest agent message aloud once the audio path exists.

### Device App

- Move inline Lua from `HaloService.mainLuaSource()` to `assets/halo/main.lua`
  once `HalosideApp` has an asset-loading API.

### SDK Deferred Items

- `TxTextPage` + `CircularTextLayout`: needs a font rasterization engine.
- `TxCaptureSettings` / `TxAutoExpSettings`: camera config structs.
- `RxTap` / `RxMeteringData`: Frame-specific.
- LC3 codec: needed for Halo speaker audio.
- DFU/OTA firmware update: SMP protocol.

### Audio

- Receive Halo mic audio via Lua RX, decode it, and feed it to the voice session.
- Send TTS/speaker audio to Halo via LC3 over audio TX.
- Route audio without assuming Android `AudioRecord`/`AudioTrack` phone devices.

## Target Display Mapping

| caic task state | Halo display |
|-----------------|--------------|
| `running` | Spinner + "Running" + task count |
| `waiting` | Yellow dot + "Awaiting input" + task title |
| `asking` | Question mark + "Asking" + task title |
| `has_plan` | Plan icon + "Plan ready" + task title |
| `failed` | Red X + "Failed" + error snippet |
| `purging` / `pushed` | Dimmed + task title |
| `purged` | Checkmark flash, then task count |

Halo's display is 256x256 and round. Keep text compact; use `TxSprite` for state
badges and `TxPlainText` for labels.

## Open Questions

1. **LC3 codec:** `liblc3` through JNI or a pure Kotlin implementation.
2. **Device bonding UX:** Android may show repeated pairing dialogs; UI should
   expose a clear "Waiting for pairing..." state.
3. **Button thresholds:** hardware click timing may need settings if Brilliant SDK
   defaults are not reliable.

## Reference Sources

| Resource | Path |
|----------|------|
| Brilliant docs | `https://github.com/brilliantlabsAR/docs` |
| Brilliant SDK (Dart) | `https://github.com/brilliantlabsAR/brilliant_sdk/tree/main/flutter/packages` |
| Brilliant SDK (Python) | `https://github.com/brilliantlabsAR/brilliant_sdk/tree/main/python/packages` |
| Halo emulator | `https://github.com/brilliantlabsAR/brilliant_sdk/tree/main/python/packages/halo_emulator` |
| Frame-2 firmware | Private repo, not cloned |
