# Debugging with the Android Emulator

Guide for debugging the caic Android app when a physical device is unavailable.

## Prerequisites

- A host that can run the Android emulator. On Linux, `/dev/kvm` must exist for
  hardware acceleration.
- The caic backend running (real or fake mode)

## Start The Emulator

The start target installs required SDK packages, creates the AVD if needed, then
starts the emulator and waits for boot:

```bash
make android-start-emulator
```

The start script runs the emulator with these flags:

```bash
$ANDROID_HOME/emulator/emulator -avd caic_test \
    -no-window -no-audio -gpu swiftshader_indirect -no-boot-anim -wipe-data &
```

Flags:
- `-no-window`: no GUI (required in headless/SSH environments)
- `-no-audio`: skip host audio (emulator mic sends silence; see Limitations)
- `-gpu swiftshader_indirect`: software rendering (works without GPU passthrough)
- `-no-boot-anim`: faster boot
- `-wipe-data`: clean state each time

Check the connected device when debugging adb state:

```bash
adb devices  # should show <device-id>
```

Stop the emulator:

```bash
make android-stop-emulator
```

Or manually:

```bash
adb emu kill
```

## Grant Permissions Upfront

Skip runtime permission dialogs:

```bash
adb -s <device-id> shell pm grant com.fghbuild.caic android.permission.RECORD_AUDIO
adb -s <device-id> shell pm grant com.fghbuild.caic android.permission.POST_NOTIFICATIONS
```

## Set Up Port Forwarding

The emulator needs to reach the backend. Use `adb reverse` so `localhost:2242`
inside the emulator maps to the host:

```bash
adb -s <device-id> reverse tcp:2242 tcp:2242
```

Alternative: the emulator maps `10.0.2.2` to the host, so
`http://10.0.2.2:2242` also works as a server URL.

## Build, Install, and Launch

```bash
# Build (use --no-daemon to avoid stale Gradle lock files)
cd android && ./gradlew assembleDebug --no-daemon

# Install
adb -s <device-id> install -r caic/build/outputs/apk/debug/caic-debug.apk

# Launch
adb -s <device-id> shell am start -n com.fghbuild.caic/.MainActivity
```

If a physical device is also connected, always pass `-s emulator-5554` to target
the emulator. Without it, `adb` fails with "more than one device/emulator".

## Configure the App

The server URL must be set in Settings before the app is functional.

**Option A — via the UI**: Tap the gear icon, enter `http://localhost:2242`, tap
"Test Connection" to verify.

**Option B — via uiautomator** (scriptable):

```bash
# Dump UI to find element bounds
adb -s <device-id> shell uiautomator dump /sdcard/ui.xml
adb -s <device-id> shell cat /sdcard/ui.xml | grep -oP 'text="[^"]*"[^>]*bounds="[^"]*"'

# Tap elements by their bounds center coordinates
adb -s <device-id> shell input tap <x> <y>
adb -s <device-id> shell input text "http://localhost:2242"
```

## Taking Screenshots

```bash
adb -s <device-id> exec-out screencap -p > /tmp/screenshot.png
```

## Capturing Logs

### Crashes

```bash
adb -s <device-id> logcat -d | grep -A 30 "FATAL\|AndroidRuntime"
```

### Voice session logs

```bash
# All VoiceSession tagged logs
adb -s <device-id> logcat -s "VoiceSession:*"

# Broader: include OkHttp and errors
adb -s <device-id> logcat | grep -E "VoiceSession|WebSocket|Error"

# App-process only (warnings and errors)
PID=$(adb -s <device-id> shell pidof com.fghbuild.caic)
adb -s <device-id> logcat -d | grep "$PID" | grep -E " W | E "
```

### Clear and capture fresh

```bash
adb -s <device-id> logcat -c          # clear
# ... reproduce the issue ...
adb -s <device-id> logcat -d          # dump since last clear
```

## Voice Mode on the Emulator

The voice connection (WebSocket to Gemini Live API) **works** on the emulator.
The connection flow succeeds:

1. Token fetch from backend
2. WebSocket open to `wss://generativelanguage.googleapis.com/ws/...`
3. Setup message sent
4. `setupComplete` received
5. Audio pipeline initialized
6. UI shows "Listening..."

### Limitations

- **No real microphone**: the emulator's AudioRecord produces silence, so
  Gemini's server-side VAD never detects speech. You cannot test a full voice
  conversation on the emulator.
- **No audio output**: with `-no-audio`, playback goes nowhere. Even without
  that flag, emulator audio is unreliable.
- **`CALL_AUDIO_INTERCEPTION` warning**: the `routeToBluetoothScoIfAvailable()`
  call triggers a harmless permission warning in logcat. This is expected.

### What you CAN test on the emulator

- WebSocket connection establishment and `setupComplete`
- WebRTC signaling flow (`POST /api/voicegateway/v1/voice/rtc/offer`)
- Error handling (invalid API key, missing server URL, network errors)
- Text injection via `injectText()` (proactive task notifications)
- Function call dispatch (if triggered by text input to the session)
- UI state transitions (connecting, listening, error states)
- Voice overlay rendering

### What REQUIRES a physical device

- Full voice conversation (speech-to-speech)
- VAD (voice activity detection) behavior
- Bluetooth SCO audio routing
- Audio quality and latency
- Barge-in behavior

## Running the Backend in Fake Mode

For testing without real containers:

```bash
make fake-dev
```

This starts the server with a fake agent backend and a temp git repo. The voice
token endpoint works normally (returns the real `GEMINI_API_KEY` from env).

## Debugging the Gemini Live API Directly

To isolate API issues from the Android app, test the WebSocket connection
directly with Python:

```bash
uv tool run --with websockets python3 <<'EOF'
import asyncio, json, os, websockets

API_KEY = os.environ["GEMINI_API_KEY"]
URL = (
    "wss://generativelanguage.googleapis.com/ws/"
    "google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
    f"?key={API_KEY}"
)

async def test():
    async with websockets.connect(URL, open_timeout=10) as ws:
        await ws.send(json.dumps({"setup": {
            "model": "models/gemini-2.5-flash-native-audio-preview-12-2025",
            "generationConfig": {"responseModalities": ["AUDIO"]},
        }}))
        msg = json.loads(await ws.recv())
        print("setupComplete" in msg and "OK" or f"Unexpected: {list(msg.keys())}")

asyncio.run(test())
EOF
```

If this prints "OK", the API key and model are valid. If it fails, the issue is
with the key or network, not the app.

## Common Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| "API key not valid" on WebRTC close | Backend's `GEMINI_API_KEY` is wrong or expired | Verify the backend env var and restart the backend |
| "Voice auth failed" error in app | `GEMINI_API_KEY` not set on backend | Set the env var and restart the backend |
| "Server URL is not configured" | Empty server URL in app settings | Configure in Settings screen |
| Gradle lock timeout | Stale Gradle daemon holding locks | `pkill -f GradleDaemon; find ~/.gradle/caches -name '*.lock' -delete` |
| "more than one device/emulator" | Physical device + emulator both connected | Use `-s <device-id>` with all adb commands |
| App shows "Listening..." but no response | Emulator mic sends silence; VAD never triggers | Expected — use text injection or physical device |

## Suggested Improvements

1. **`make android-emulator` target**: automate AVD creation, system image
   install, and headless emulator launch in a single command.

2. **`make android-push-emulator` target**: build + install + configure server
   URL + grant permissions, targeting the emulator specifically. Would eliminate
   the manual setup steps.

3. **Scriptable server URL configuration**: add an intent extra or a debug-only
   broadcast receiver that sets the server URL without touching the UI:
   ```bash
   adb shell am broadcast -a com.fghbuild.caic.SET_SERVER_URL \
       --es url "http://localhost:2242"
   ```

4. **Instrumented UI tests**: Espresso or Compose UI tests that verify the voice
   connection flow on the emulator (up to `setupComplete`), without needing a
   real mic.

5. **Log the token endpoint URL**: when the voice connection fails, log which
   server URL was used for the token fetch, making it easier to diagnose
   misconfiguration.
