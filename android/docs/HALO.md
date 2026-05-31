# Halo Device Support — Investigation Notes

Supporting the [Brilliant Halo](https://docs.brilliant.xyz/halo/halo/) smart glasses
over Bluetooth LE from the caic Android app (Kotlin/Compose).

## Key Architecture Takeaways

**Halo is a BLE peripheral**, not a standalone computing device. A host app (phone/desktop)
drives it via Bluetooth LE — sending Lua commands, exchanging typed messages, streaming
audio, and receiving sensor data. Halo itself runs Zephyr OS + Lua 5.3 VM on an Alif
Balletto B1 (Cortex-M55 + Ethos-U55 NPU).

**Two-layer protocol:**

```
Host App (caic Android)
  └── application messaging (sprites, text, audio, IMU, photos, clicks)
      └── BLE transport (connect, MTU-aware chunking, DFU)
          └── Bluetooth LE
              └── Halo device (Lua VM + frame.* API)
```

The Brilliant SDK has implementations in Python, Flutter, and Web Bluetooth (TypeScript).
No native Android/Kotlin SDK exists. We would build one.

---

## BLE Protocol Details

### Service & Characteristics

| Service | UUID | Purpose |
|---------|------|---------|
| Lua Service | `7A230001-5475-A6A4-654C-8431F6AD49C4` | Main data channel |
| LUA TX char | `7A23...002` (Write, WriteWithoutResponse) | Host → Halo: Lua strings, raw data `0x01` prefix, control bytes |
| LUA RX char | `7A23...003` (Notify) | Halo → Host: `print()` output, raw data `0x01` prefix |
| AUDIO TX char | `7A23...005` (Write, WriteWithoutResponse) | Host → Halo: PCM/LC3 audio to speaker |
| Battery Service | `0x180F` / `0x2A19` | Standard BLE Battery Level (Read, Notify) |
| OTA Service | `8D53DC1D-...` | SMP/MCUboot firmware updates |

**Device naming:** `Halo XX` where `XX` is byte 4 of EUI-48 MAC (hex).

**MTU:** Up to 512 bytes negotiated.

### Data Framing

Three kinds of traffic on the LUA TX/RX characteristics, distinguished by first byte:

| Byte 0 | Type | Description |
|--------|------|-------------|
| `0x01` | Raw binary data | `frame.bluetooth.send()` / `receive_callback()` |
| `0x02`–`0x07` | Control signals | Break, reset, reboot, etc. |
| Anything else | Lua string | REPL commands, `print()` output |

**Control bytes:**
- `0x03` — Break (interrupt running script) — most commonly used
- `0x04` — Reset Lua VM / run `main.lua`
- `0x05` — Remove `main.lua` (Halo-only)
- `0x02` — Reboot device
- `0x06` — Exit Lua runtime
- `0x07` — Remove all files

### Typed Message Protocol (`sendMessage`)

For application-level messaging (sprites, text, audio, IMU, photos), Brilliant uses
a chunked message protocol over the raw data (`0x01`) channel:

- **First packet:** `[0x01] [msgCode] [length_high] [length_low] [payload...]` (4-byte header)
- **Subsequent packets:** `[0x01] [msgCode] [payload...]` (2-byte header)
- **Max payload:** 65535 bytes
- Each chunk is ≤ MTU − 4 bytes (first) or MTU − 2 bytes (subsequent)
- Application-level ACK: device responds with `[0x01] [msgCode] [0x00]` (or `[0x01] [msgCode] [0x01]` for error)

This is how `brilliant_msg` works. The device-side Lua `data.lua` library reassembles
packets and dispatches by msgCode.

---

## Halo-Specific Features (vs Frame)

These matter for caic integration — what could a coding agent *do* with a wearable?

| Feature | Halo | Frame | caic Relevance |
|---------|------|-------|----------------|
| Display | 256×256 round, no double-buffer | 640×400, double-buffered | Show coding output, diffs, status |
| Audio output | Speaker (PCM/LC3) | — | Voice assistant responses, agent audio |
| Button | Single/double/long press | IMU tap | Quick actions without phone |
| Camera | 640×480 global shutter, libmpix pipeline | 640×400 rolling shutter | Screenshot capture, code scanning |
| Microphone | Stereo, AAD wake detection | Mono | Voice input to agents |
| IMU | Accel + e-compass (heading) | Accel only | — |
| Sleep modes | sleep/standby/light_sleep/ship | sleep only | Battery management |

---

## What We'd Need to Build

### 1. Native Android BLE Layer (Kotlin)

Replaces `brilliant_ble` (Flutter/Dart) with pure Android BLE APIs (`android.bluetooth.le`):

- **Scanning:** `BluetoothLeScanner` with service UUID filter `7A230001-...`
- **Connection:** `BluetoothGatt` connect + bond + negotiate MTU 517
- **GATT operations:** Write on TX char, notifications on RX char
- **Data framing:** Split Lua strings and raw data into MTU-sized chunks
- **Response handling:** Coroutine-based `Flow<ByteArray>` for incoming data,
  separating string responses vs raw data by `data[0]` byte
- **Control signals:** Break/reset/remove as single-byte writes
- **Audio TX:** Dedicated characteristic with `writeWithoutResponse` for PCM/LC3 streaming
- **Battery:** Standard BLE Battery Service read + notify
- **Reconnection:** Handle Android BLE bonding lifecycle

**Key Android permissions needed:** `BLUETOOTH_CONNECT`, `BLUETOOTH_SCAN` (we already have `BLUETOOTH_CONNECT`)

**Dependencies:** None beyond Android SDK — `android.bluetooth` + coroutines. No third-party BLE library needed.

**minSdk = 33 implications (favorable):**
- BLE permissions (`BLUETOOTH_CONNECT` / `BLUETOOTH_SCAN`, API 31) are guaranteed — no legacy `BLUETOOTH` / `BLUETOOTH_ADMIN` fallback needed.
- `ScanFilter.Builder.setServiceUuid()` available (API 21, but reliable from 26+).
- `BluetoothGatt.requestConnectionPriority(CONNECTION_PRIORITY_HIGH)` for 2M PHY.
- `BluetoothDevice.setConnectionPriority()` no longer deprecated.
- No need to support pre-API-31 pairing flow quirks.

### 2. Messaging Protocol Layer (Kotlin)

Replaces `brilliant_msg`:
- `TxMessage` / `RxMessage` base types
- Chunked send with ACK — `sendMessage(msgCode: Int, payload: ByteArray)`
- Message type definitions: `TxSprite`, `TxTextPage`, `RxPhoto`, `RxIMU`, `RxClick`, `RxAudio`
- Sprite encoding: PNG → indexed color, palette → wire format
- Text rasterization: text → `CircularTextLayout` for 256×256 round display
- Photo reassembly: JPEG chunks → complete file

### 3. Lua File Upload

- Escape Lua strings correctly (backslash, newline, quote handling)
- Write to device filesystem via `frame.file.open/write/close` over REPL
- Upload standard libraries: `data.min.lua`, `sprite.min.lua`, `plain_text.min.lua`, etc.

### 4. Haloside App Pattern

The standard app pattern (from `brilliant_msg` / `simple_brilliant_app`):
1. Send break + reset + break to ensure REPL mode
2. Upload Lua libraries to device filesystem
3. Upload main Lua app loop
4. Start loop (device polls for messages, host sends typed messages)
5. Exchange messages asynchronously

---

## Potential caic Integration Ideas

| Use Case | How Halo Helps |
|----------|---------------|
| **Voice agent** | Use Halo mic/speaker for Gemini Live instead of phone — true hands-free |
| **Status display** | Show task state, error snippets, token counts on Halo display |
| **Quick actions** | Button click → ask agent for status, double-click → purge task |
| **Code review on the go** | Stream diffs to display (text pages via `TxTextPage`) |
| **Screenshot capture** | Halo camera → send to agent for context ("what do you see?") |
| **Notifications** | Flash display on task state changes |

---

## Implementation Strategy

### Phase 1: Transport Layer (Kotlin BLE module)

A new Gradle module (e.g., `:halo-ble` or a package in the SDK module) providing:
- `HaloScanner` — scan for `Halo XX` devices
- `HaloConnection` — connect, bond, negotiate MTU, discover services
- `HaloTransport` — send Lua, send data, send control signals, observe responses
- `HaloAudio` — stream PCM/LC3 to AUDIO TX characteristic

**No dependencies on caic business logic.** Pure BLE transport, could live in a
separate module or even its own library.

### Phase 2: Messaging Layer

- `HaloMessenger` — typed message send/receive over the transport
- Message types mirroring `brilliant_msg`
- Lua file upload helpers

### Phase 3: App Integration

- Compose UI for device scanning, pairing, connection state
- Voice mode: route mic/speaker through Halo
- Screen mode: display task status on Halo

### Open Questions

1. **Audio routing:** Android audio APIs (AudioRecord/AudioTrack) are currently
   hardcoded to phone mic/speaker. To use Halo mic/speaker, we'd need to:
   - Receive mic audio via BLE (LUA RX → PCM/LC3 → decode → feed to Gemini Live)
   - Send TTS/speaker audio via BLE (Gemini response → encode PCM/LC3 → AUDIO TX)
   - This is a significant audio pipeline — may justify Phase 1 being display+button only
2. **LC3 codec:** Android 13 (API 33, our minSdk) has native LE Audio support via
   `BluetoothLeAudioCodecConfig`. However this is for configuring the Bluetooth
   stack's LE Audio path — it does **not** provide app-level LC3 encode/decode.
   To send/receive raw LC3 frames over GATT (as Halo does on AUDIO TX / LUA RX),
   we'd need an LC3 library: `liblc3` (C, via JNI) or a pure Kotlin port.
   The LC3 bitstream format is standardized (ETSI TS 103 634), frame size is
   deterministic from sample rate + frame duration, so a port is feasible.
3. **Device bonding UX:** Android BLE bonding has platform-specific quirks —
   "Pair" dialog may appear twice on some Android versions (per the docs).

---

## Reference Repos and Docs

| Resource | URL | Notes |
|----------|-----|-------|
| Brilliant docs (Halo) | `~/src/docs/halo/` (cloned from `brilliantlabsAR/docs`) | Bluetooth specs, Lua API, hardware |
| Brilliant SDK | `~/src/brilliant_sdk/` (cloned from `brilliantlabsAR/brilliant_sdk`) | Python + Flutter + WebBluetooth implementations |
| Flutter BLE layer | `~/src/brilliant_sdk/flutter/packages/brilliant_ble/` | Reference for connect/scan/send logic |
| Flutter msg layer | `~/src/brilliant_sdk/flutter/packages/brilliant_msg/` | Reference for typed message protocol |
| Python BLE layer | `~/src/brilliant_sdk/python/packages/brilliant_ble/` | Clean Python reference (Bleak)
| Halo emulator | `~/src/brilliant_sdk/python/packages/halo_emulator/` | Can test Lua API without hardware |
| caic Android app | `~/src/caic-xyz/caic/android/` | Our target app |
| caic Android docs | `~/src/caic-xyz/caic/android/docs/` | App design, SDK design |
