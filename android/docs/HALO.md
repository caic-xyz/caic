# Halo Device Support

Supporting the [Brilliant Halo](https://docs.brilliant.xyz/halo/halo/) smart glasses
over Bluetooth LE from Go Mode and shared Android SDK code:

- `:gomode` (`com.fghbuild.gomode`) owns shell-level Halo device management and
  must stay independent of caic domain APIs.
- Shared BLE transport and typed messaging code belongs in `:halo-sdk`.

## Architecture Boundary

```
Android modules
  ├── :gomode
  │   └── com.fghbuild.gomode.halo.HaloController  shell-owned scan/connect/disconnect
  └── :halo-sdk
      ├── com.caic.halo.msg.HalosideApp            device-side app lifecycle
      └── com.caic.halo.ble                        BLE transport
```

Protocol details live in [sdk/halo/PROTOCOL.md](../../sdk/halo/PROTOCOL.md).
SDK development notes live in [sdk/halo/AGENTS.md](../../sdk/halo/AGENTS.md).

## Halo Lua Emulator

[`scripts/halo-emulator.py`](../../scripts/halo-emulator.py) is a self-contained
`uv` script with inline Python dependencies. It runs Brilliant Labs'
`halo_emulator`, which emulates the Halo Lua runtime and `frame.*` APIs. It does
**not** advertise as a BLE peripheral, so Android scan/connect flows still
require real hardware or a future BLE-peripheral shim.

Use it for haloside Lua development: display rendering, button/tap injection,
BLE payload injection, and host-message tests through Brilliant's
`EmulatorBrilliantMsg` adapter.

```bash
scripts/halo-emulator.py path/to/lua-app --script main.lua
scripts/halo-emulator.py path/to/lua-app --script main.lua --headless
```

For Android integration work, run the emulator as a WebSocket bridge:

```bash
scripts/halo-emulator.py --bridge 0.0.0.0:8765 path/to/lua-app --script main.lua
```

Android emulator clients reach the host bridge at `ws://10.0.2.2:8765`. A
physical Android device can use `adb reverse tcp:8765 tcp:8765` and connect to
`ws://127.0.0.1:8765`. The bridge speaks JSON request/response messages and
emits async events:

```json
{"id":1,"op":"ping"}
{"id":2,"op":"send_message","msgCode":16,"payload":"SGVsbG8="}
{"id":3,"op":"button_single"}
{"event":"bluetooth_sent","data":"...base64..."}
```

Supported bridge operations: `ping`, `connect_repl`, `execute_lua`, `start`,
`stop`, `break`, `reset`, `remove_all_files`, `upload_file`, `clear_display`,
`send_message`, `button_single`, `button_double`, `button_long`, `imu_tap`, and
`get_framebuffer`. Android code can use `HaloEmulatorBridgeClient` from
`:halo-sdk` instead of writing raw WebSocket JSON.

Use the emulator against Go Mode-owned Lua assets or upstream examples. Add
emulator-backed tests for display output and click message payloads once Go Mode
ships a device-side app.

## Remaining Work

### Go Mode Integration

- Auto-reconnect: observe `SettingsRepository.haloAddress` and reconnect on app
  start when auto-connect is enabled.
- Service display: keep device output service-neutral unless the hosted frontend
  exposes a shell capability for richer status.
- Double click: trigger a shell-owned voice action once the audio path exists.

### Device App

- Move Go Mode Lua assets to `assets/halo/main.lua` once `HalosideApp` has an
  asset-loading API.
- Add emulator-backed Lua tests using `halo_emulator` once the device app lives
  in assets: assert framebuffer output and button/click payloads without
  requiring Halo hardware.

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

| Service-neutral state | Halo display |
|-----------------------|--------------|
| Active | Spinner + concise status |
| Attention needed | Yellow dot + hosted-service label |
| Question | Question mark + hosted-service label |
| Failed | Red X + error snippet |
| Complete | Checkmark flash |

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
| Halo emulator runner | [`scripts/halo-emulator.py`](../../scripts/halo-emulator.py) |
| Frame-2 firmware | Private repo, not cloned |
