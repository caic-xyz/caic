# Halo Device Support

Supporting the [Brilliant Halo](https://docs.brilliant.xyz/halo/halo/) smart glasses
over Bluetooth LE from the caic Android app (Kotlin/Compose).

## Status

| Phase | Status |
|-------|--------|
| 1. Transport layer | ✅ Done — `:halo` module, `com.caic.halo.ble` package |
| 2. Messaging layer | ✅ Done — `com.caic.halo.msg` package, merged into `:halo` |
| 3. App integration | Not started |

## Architecture

```
caic Android App
  └── com.caic.halo.msg  —  typed messages (sprites, text, IMU, photos, clicks, audio)
      └── com.caic.halo.ble  —  BLE transport (connect, MTU-aware chunking, Lua REPL)
          └── Android Bluetooth LE
              └── Halo device (Lua VM + frame.* API)
```

**Module:** `:halo` at `sdk/halo/`. 68 JVM unit tests, 0 failures. Zero third-party
BLE libraries — pure `android.bluetooth.le`.

**Protocol reference:** [sdk/halo/PROTOCOL.md](../../sdk/halo/PROTOCOL.md) — full BLE
services, data framing, typed message wire formats, source-to-code mappings.

**Development guide:** [sdk/halo/AGENTS.md](../../sdk/halo/AGENTS.md) — architecture,
design decisions, test strategy, deferred items.

## What's Built

### Transport (`com.caic.halo.ble`)

- `HaloDevice` — Lua REPL, raw data (`0x01`), control signals, audio TX, typed
  message chunking (sendMessage), file upload, response streams
- `HaloConnection` — BLE scan, connect (bond + MTU 517), service discovery,
  characteristic binding, Halo vs Frame detection, disconnect
- `HaloServiceDiscovery` — pure functions for UUID matching, device type detection,
  characteristic extraction, MTU payload limits

### Messaging (`com.caic.halo.msg`)

- **Tx:** `TxSprite` (PNG→indexed, 1/2/4bpp), `TxPlainText`, `TxCode`, `TxMessage`
- **Rx:** `RxClick` (single/double/long), `RxIMU` (6×float32), `RxPhoto` (JPEG reassembly),
  `RxAudio` (PCM/LC3 reassembly)
- `HalosideApp` — lifecycle: break/reset/break → upload libraries → upload main.lua → start loop

### Deferred

- `TxTextPage` + `CircularTextLayout` — needs font rasterization engine
- `TxCaptureSettings` / `TxAutoExpSettings` — complex camera config structs
- `RxTap` / `RxMeteringData` — Frame-specific
- LC3 codec — needs `liblc3` via JNI; PCM suffices for now
- DFU/OTA firmware update — SMP protocol

## Phase 3: App Integration

### Device management
- Compose UI for BLE scanning, pairing, connection state
- Permission request flow (`BLUETOOTH_SCAN`, `BLUETOOTH_CONNECT`)
- Background reconnect

### Halo as caic peripheral
- **Status display:** Show task state, error snippets, token counts on Halo's
  256×256 round display via `TxPlainText` / `TxSprite`
- **Quick actions:** Button click (single/double/long) → ask agent status, purge task, etc.
- **Notifications:** Flash display on task state changes

### Audio (significant work, may defer)
- Receive Halo mic audio via LUA RX → decode → feed to Gemini Live
- Send TTS/speaker audio → encode LC3 → AUDIO TX
- Requires LC3 codec (`liblc3` via JNI or pure Kotlin port)

### Open Questions

1. **Audio routing:** Currently `AudioRecord`/`AudioTrack` are hardcoded to phone
   mic/speaker. BLE audio pipeline is a separate path entirely.
2. **LC3 codec:** ETSI TS 103 634 standard. Frame size deterministic from sample
   rate + frame duration. `liblc3` (C) is ~600 lines, straightforward JNI port.
3. **Device bonding UX:** Android BLE bonding quirks — "Pair" dialog may appear
   twice on some Android versions.

## Reference Sources

| Resource | Path |
|----------|------|
| Brilliant docs | `~/src/docs/halo/` (cloned from `brilliantlabsAR/docs`) |
| Brilliant SDK (Dart) | `~/src/brilliant_sdk/flutter/packages/brilliant_{ble,msg}/` |
| Brilliant SDK (Python) | `~/src/brilliant_sdk/python/packages/brilliant_{ble,msg}/` |
| Halo emulator | `~/src/brilliant_sdk/python/packages/halo_emulator/` |
| Frame-2 firmware | Private repo — not cloned |
