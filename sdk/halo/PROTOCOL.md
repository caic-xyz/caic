# Halo Protocol Reference

Wire-level details for the Brilliant Halo BLE protocol. This is the reference
used to build `com.caic.halo.ble` and `com.caic.halo.msg`. Generated from the
Brilliant SDK sources and docs.

## Reference Sources

### Brilliant Labs Documentation (`~/src/docs/`)

Cloned from `https://github.com/brilliantlabsAR/docs`.

| File | Used for |
|------|----------|
| `halo/halo-sdk-bluetooth-specs.md` | Complete BLE protocol: service UUIDs, characteristics, data framing (0x01 prefix, control bytes 0x02–0x07), typed message protocol, audio TX, OTA/SMP, MTU, pairing |
| `halo/halo-sdk-lua.md` | Lua API reference: `frame.display.*`, `frame.camera.*`, `frame.microphone.*`, `frame.speaker.*`, `frame.bluetooth.*`, `frame.imu.*`, `frame.file.*`, `frame.button.*`, libmpix pipeline |
| `halo/halo-sdk-flutter.md` | Flutter SDK overview — `brilliant_ble` vs `brilliant_msg` separation, startup sequence, msgCode constants (0x20 for sprites, 0x0B for clicks, 0x0A for IMU, 0x05/0x06 for audio, 0x07/0x08 for photos) |
| `halo/hardware.md` | Hardware: Balletto B1 MCU, VGA020 display (256×256 round), PAG7982J1 camera, T5838 mics, BMA580/QMC6308 IMU, TPA2011D1 amp, BQ25170 charger |

### Brilliant SDK Source (`~/src/brilliant_sdk/`)

Cloned from `https://github.com/brilliantlabsAR/brilliant_sdk`.

#### Flutter BLE layer — `flutter/packages/brilliant_ble/lib/`

| File | Kotlin equivalent |
|------|-------------------|
| `brilliant_device.dart` | `HaloDevice` — `sendString()`, `sendData()`, `sendBreakSignal()`, `sendResetSignal()`, `sendRemoveSignal()`, `sendMessage()`, `uploadScript()`, `clearDisplay()`, `isLuaInReplState()`, `sendAudio()`, `stringResponse`/`dataResponse` streams |
| `brilliant_bluetooth.dart` | `HaloConnection` — `scan()`, `connect()`, `enableServices()`, `reconnect()`, `getSystemConnectedDevice()`, service discovery, characteristic binding, device type detection (AUDIO_TX → HALO) |
| `brilliant_scanned_device.dart` | `HaloScannedDevice` |
| `brilliant_bluetooth_exception.dart` | `HaloException` |

#### Flutter messaging layer — `flutter/packages/brilliant_msg/lib/`

| File | Kotlin equivalent |
|------|-------------------|
| `tx_msg.dart` | `TxMessage` interface |
| `tx/sprite.dart` | `TxSprite` — PNG→indexed, palette, 1/2/4bpp packing, wire format |
| `tx/plain_text.dart` | `TxPlainText` — x+y+paletteOffset+spacing+UTF-8 |
| `tx/code.dart` | `TxCode` — single byte |
| `tx/text_page.dart` | `TxTextPage` + `CircularTextLayout` — deferred |
| `rx/click.dart` | `RxClick` — msgCode 0x0B, types 1/2/3 |
| `rx/imu.dart` | `RxIMU` — msgCode 0x0A, 6×float32 LE |
| `rx/photo.dart` | `RxPhoto` — msgCode 0x07/0x08, chunk reassembly |
| `rx/audio.dart` | `RxAudio` — msgCode 0x05/0x06, chunk reassembly |
| `rx/tap.dart` | `RxTap` — not ported (Frame-specific) |

#### Python BLE layer — `python/packages/brilliant_ble/src/brilliant_ble/brilliant_ble.py`

Validated sendMessage chunking, MTU calculation, escape handling in upload_file, control signal bytes.

## BLE Services

| Service | UUID |
|---------|------|
| Lua | `7A230001-5475-A6A4-654C-8431F6AD49C4` |
| Battery (standard) | `0000180F-0000-1000-8000-00805F9B34FB` |
| OTA (MCUboot/SMP) | `8D53DC1D-1DB7-4CD3-868B-8A527460AA84` |

## Characteristics (on Lua service)

| Char | UUID | Permissions |
|------|------|-------------|
| LUA TX | `...-002` | Write, WriteWithoutResponse |
| LUA RX | `...-003` | Notify |
| AUDIO TX | `...-005` | Write, WriteWithoutResponse |

## Data Framing (on LUA TX/RX)

| Byte 0 | Type |
|--------|------|
| `0x01` | Raw binary data (for `receive_callback` / `bluetooth.send`) |
| `0x02` | Reboot |
| `0x03` | Break (interrupt Lua script) |
| `0x04` | Reset (restart Lua VM, run `main.lua`) |
| `0x05` | Remove `main.lua` (Halo-only) |
| `0x06` | Exit Lua runtime |
| `0x07` | Remove all files |
| other | Lua string (REPL command or `print()` output) |

## Typed Message Protocol

- First packet: `[0x01] [msgCode] [len_hi] [len_lo] [payload…]` — 4-byte header
- Subsequent: `[0x01] [msgCode] [payload…]` — 2-byte header
- Max payload: 65535 bytes
- Chunk size: ≤ MTU − 4 (first) or MTU − 2 (subsequent)
- Device ACKs each chunk with `[0x01] [msgCode] [0x00]`
- Lua `data.min.lua` reassembles and dispatches by msgCode

## Message Wire Formats

**TxSprite** (msgCode varies, typically 0x20–0x2F):
```
width(2) height(2) compressed(1) bpp(1) numColors(1) palette(3×N) pixels(packed)
```

**TxPlainText** (msgCode varies):
```
x(2) y(2) paletteOffset(1) spacing(1) text(UTF-8)
```

**TxCode** (msgCode varies):
```
value(1)
```

**RxClick** (msgCode 0x0B):
```
[0x0B, type]  // 1=single, 2=double, 3=long
```

**RxIMU** (msgCode 0x0A):
```
[0x0A, 24 bytes: 6×float32 LE]  // compassX,Y,Z, accelX,Y,Z
```

**RxPhoto** (msgCode 0x07 non-final, 0x08 final):
```
[0x07, chunk…] … [0x08, finalChunk…]  → reassembled JPEG
```

**RxAudio** (msgCode 0x05 non-final, 0x06 final):
```
[0x05, chunk…] … [0x06, finalChunk…]  → reassembled audio
```

## MTU Payload Limits

```
maxStringLen = mtu - 3        (ATT overhead)
maxDataLen   = mtu - 4        (ATT + 0x01) — Frame
maxDataLen   = mtu - 6        (ATT + 0x01 + audio TX coexistence) — Halo
```
