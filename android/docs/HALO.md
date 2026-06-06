# Halo Device Support

Supporting the [Brilliant Halo](https://docs.brilliant.xyz/halo/halo/) smart glasses
over Bluetooth LE from the caic Android app (Kotlin/Compose).

## Status

| Phase | Status |
|-------|--------|
| 1. Transport layer | ✅ Done — `:halo-sdk` module, `com.caic.halo.ble` package |
| 2. Messaging layer | ✅ Done — `com.caic.halo.msg` package, merged into `:halo-sdk` |
| 3. App integration | 🟡 In progress — DI wiring, HaloService bridge, pure-function tests done |

## Architecture

```
caic Android App
  └── com.fghbuild.caic.halo.HaloService   — bridge: task state → Halo display, clicks → actions
      └── com.caic.halo.msg.HalosideApp    — orchestrates Halo device lifecycle
          └── com.caic.halo.ble            — BLE transport
              └── Android Bluetooth LE
                  └── Halo device (Lua VM + frame.* API)
```

**Module:** `:halo-sdk` at `sdk/halo/`. 68 JVM unit tests, 0 failures. Zero third-party
BLE libraries — pure `android.bluetooth.le`.

**Protocol reference:** [sdk/halo/PROTOCOL.md](../../sdk/halo/PROTOCOL.md) — full BLE
services, data framing, typed message wire formats, source-to-code mappings.

**Development guide:** [sdk/halo/AGENTS.md](../../sdk/halo/AGENTS.md) — architecture,
design decisions, test strategy, deferred items.

## What's Built

### Transport (`com.caic.halo.ble`)

- `HaloDevice` — Lua REPL, raw data (`0x01`), control signals, audio TX, typed
  message chunking (sendMessage), file upload, response streams.
- `HaloConnection` — BLE scan, connect (bond + MTU 517), service discovery,
  characteristic binding, Halo vs Frame detection, disconnect
- `HaloServiceDiscovery` — pure functions for UUID matching, device type detection,
  characteristic extraction, MTU payload limits

### Messaging (`com.caic.halo.msg`)

- **Tx:** `TxSprite` (PNG→indexed, 1/2/4bpp), `TxPlainText`, `TxCode`, `TxMessage`
- **Rx:** `RxClick` (single/double/long via `attach()`), `RxIMU` (6×float32),
  `RxPhoto` (JPEG reassembly), `RxAudio` (PCM/LC3 reassembly)
- `HalosideApp` — lifecycle: break/reset/break → upload libraries → upload main.lua → start loop

### App Integration (`com.fghbuild.caic.halo` + `com.fghbuild.caic.di`)

- `di/HaloModule.kt` — Hilt module providing `HaloConnection` singleton
- `halo/HaloService.kt` — core bridge: observes `TaskRepository.tasks` via StateFlow,
  computes diffs, sends status updates to Halo, listens for `RxClick` events.
  Companion object houses pure functions (`primaryTask`, `stateLabel`,
  `buildStatusString`, `diffTasks`) — all covered by 15 unit tests.
- `halo/HaloServiceTest.kt` — 15 tests for pure functions: attention task selection,
  state labels (all 15 variants + Other), status string formatting (truncation,
  pluralization, null handling), state change diff detection

### Deferred (SDK)

- `TxTextPage` + `CircularTextLayout` — needs font rasterization engine
- `TxCaptureSettings` / `TxAutoExpSettings` — complex camera config structs
- `RxTap` / `RxMeteringData` — Frame-specific
- LC3 codec — needs `liblc3` via JNI; PCM suffices for now
- DFU/OTA firmware update — SMP protocol

## Phase 3 Remaining Tasks

### Device management
- `halo/HaloViewModel.kt` — screen state: scan results, connection, bonded address, auto-connect toggle
- `ui/halo/HaloScreen.kt` — Compose UI: scan list, connect/disconnect, connection status
- `navigation/Screen.kt` — add `Screen.Halo` route
- `CaicNavGraph.kt` — wire Halo screen into both CompactLayout and WideLayout
- `data/SettingsRepository.kt` — persist `haloAddress`, `haloAutoConnect`
- Permission request flow (already declared in manifest)

### Halo as caic peripheral
- `HaloService.cycleAttentionTask()` / `purgeCurrentTask()` — wire click actions
  to TaskRepository API calls
- Auto-reconnect: observe `SettingsRepository.haloAddress`, attempt reconnect on start
- Task state → display mapping: use `TxSprite` for state badges (currently text-only
  via `TxPlainText`)
- Notification suppression while Halo connected (update `TaskNotifier`)

### Halo main.lua
- Device-side Lua app code is inline in `HaloService.mainLuaSource()`. Needs to be
  split into `assets/halo/main.lua` once the HalosideApp asset-loading API is finalised.

### Audio (deferred)
- Receive Halo mic audio via LUA RX → decode → feed to Gemini Live
- Send TTS/speaker audio → encode LC3 → AUDIO TX
- Requires LC3 codec (`liblc3` via JNI or pure Kotlin port)

## Task State → Halo Display Mapping

| caic task state | Halo display |
|-----------------|-------------|
| `running` | Spinner + "Running" + task count (e.g., "3 tasks") |
| `waiting` | Yellow dot + "Awaiting input" + task title |
| `asking` | Question mark + "Asking" + task title |
| `has_plan` | Plan icon + "Plan ready" + task title |
| `failed` | Red X + "Failed" + error snippet |
| `purging`/`pushed` | Dimmed + task title |
| `purged` (success) | Checkmark flash → back to task count |

Halo's 256×256 round display with up to ~40 chars per line (using the smallest
internal font). Use `TxSprite` for icons (state badges), `TxPlainText` for labels.

## Quick Actions (Halo Button → caic)

| Gesture | Action |
|---------|--------|
| Single click | Cycle to next attention-needed task (waiting → asking → has_plan) |
| Double click | Read latest agent message aloud via TTS (deferred: needs audio pipeline) |
| Long press | Purge current task (with confirmation via second long press within 3s) |

## Open Questions

1. **Audio routing:** `AudioRecord`/`AudioTrack` are hardcoded to phone mic/speaker.
   BLE audio pipeline is a separate path entirely.
2. **LC3 codec:** ETSI TS 103 634 standard. `liblc3` (C) is ~600 lines, straightforward
   JNI port. No pure-Kotlin LC3 encoder exists.
3. **Device bonding UX:** Android BLE bonding quirks — "Pair" dialog may appear
   twice on some Android versions. `HaloConnection` handles this but UI should show
   "Waiting for pairing…" state.
4. **Button mapping conflicts:** Some Halo hardware may use different click thresholds
   than the Brilliant SDK defaults. Configurable in settings if needed.

## Reference Sources

| Resource | Path |
|----------|------|
| Brilliant docs | `~/src/docs/halo/` (cloned from `brilliantlabsAR/docs`) |
| Brilliant SDK (Dart) | `~/src/brilliant_sdk/flutter/packages/brilliant_{ble,msg}/` |
| Brilliant SDK (Python) | `~/src/brilliant_sdk/python/packages/brilliant_{ble,msg}/` |
| Halo emulator | `~/src/brilliant_sdk/python/packages/halo_emulator/` |
| Frame-2 firmware | Private repo — not cloned |
