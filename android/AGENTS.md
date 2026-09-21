# Android Project

Kotlin/Compose Android workspace for caic clients and shared mobile modules.

Read the narrower guide before editing a module:
- `gomode/AGENTS.md` — Go Mode WebView shell app.
- `../sdk/halo/AGENTS.md` — Halo BLE/message SDK.

## Modules

- `:gomode`: Go Mode Android shell, package `com.fghbuild.gomode`.
- `:caic-sdk`: generated Kotlin caic API SDK from `sdk/caic/kotlin`.
- `:voicegateway-sdk`: generated Kotlin voice gateway API SDK from `sdk/voicegateway/kotlin`.
- `:gomode-sdk`: generated Kotlin Go Mode service discovery SDK from `sdk/gomode/kotlin`.
- `:mcp-sdk`: generated Kotlin MCP protocol SDK from `sdk/mcp/kotlin`.
- `:halo-sdk`: Halo BLE/message SDK from `sdk/halo`.

## Shared Conventions

- `minSdk = 33`, `targetSdk = 36`, `compileSdk = 36`.
- Java/Kotlin target is 17.
- Use Kotlin coroutines and `StateFlow`, not LiveData or RxJava.
- Use `kotlinx.serialization`, not Gson or Moshi.
- Use DataStore for persisted Android settings.
- Compose names stay `PascalCase`; detekt allows this.
- Line length is 120 chars; no wildcard imports.
- Do not put Android dependencies in the generated SDK module.
- Keep business logic out of Compose screens; put state and behavior in ViewModels,
  repositories, or focused service classes.

## Frontend / Android Boundary

The web frontend in `../frontend/` owns caic screen behavior, event grouping,
formatting, state colors, and widgets. Go Mode hosts that frontend in a WebView
and should not duplicate caic product UI in native Compose.

Native Android owns shell capabilities such as settings bootstrap, WebView
hosting, notifications, permissions, voice endpoint behavior, screenshot capture,
and Halo/BLE. Keep user-facing behavior aligned with the web frontend only for
shared shell capabilities.

## Build, Lint, and Test

Run the focused checks for Android changes:

```bash
make android-check
```

Kotlin is formatted and checked through `make fix` and `make verify` only when
the branch touches `.kt`/`.kts` files; `make android-check` always runs ktlint
for both modules.

Use `make verify` and `make test` for non-Android repo validation. Use
`make android-e2e` for instrumented Android flows. Use `make screenshots-check`
for deterministic visual coverage and `make screenshots-update` to accept
intentional baseline changes.

Robolectric unit tests run without network access: the test modules resolve
`org.robolectric:android-all-instrumented` through Gradle and point Robolectric's offline
resolver at the staged jar, instead of letting Robolectric download it from Maven Central
while the tests run. `robolectricAndroidAll` in `gradle/libs.versions.toml` names the jar
version; update it together with `robolectric` or `compileSdk` when either moves, because a
mismatch fails the tests with `Path is not a file: ...android-all-instrumented-<version>.jar`.

For module-focused Android e2e:

```bash
python3 scripts/android_e2e.py --module gomode
python3 scripts/android_e2e.py --module halo-sdk
```

## Emulator

```bash
make android-start-emulator
make android-stop-emulator
```

`make android-start-emulator` runs setup first. Starting the headless emulator is
cheap; for Android E2E work, start it and run the focused test instead of
skipping local validation. The dev container ships the emulator, a system image,
`ANDROID_HOME`, and `/dev/kvm`, so do not assume Android validation is
unavailable: `make android-sdk` (or `python3 scripts/android_sdk.py check`) is
the readiness probe, `adb devices` shows a running emulator, and
`make android-check`, `make android-e2e`, `make screenshots-check`, and
`make screenshots-check-android` all run here. Only the runtime containers
(`md`/podman) may be unavailable.

`make android-e2e` reuses a sole ready emulator, USB device, or Wi-Fi adb
device. Set `ANDROID_SERIAL` when more than one device is connected. The test
runner assigns temporary backend and device ports and connects them with
`adb reverse`, so it also works when an emulator runs inside a container.

For manual development with a fake caic backend:

```bash
make fake-dev
```

Use `adb reverse tcp:2242 tcp:2242` and connect to `http://localhost:2242` from
Android settings. This works for emulators and physical devices over USB or
Wi-Fi. The emulator-only `10.0.2.2` host alias may not reach the intended
network namespace when the emulator or backend runs in a container.

## UI Automation

- Use `uiautomator dump` to inspect platform view bounds and resource IDs.
- Compose nodes often lack resource IDs; prefer `testTag()` for Compose tests.
- Platform views such as WebView need real Android resource IDs for UiAutomator.
- Tap a text field before typing; text input goes to the focused field.
- Dismiss the keyboard with `adb shell input keyevent KEYCODE_BACK`.
- Clear app data with the module package name, for example
  `adb shell pm clear com.fghbuild.gomode`.

## Documentation

Run `make refresh-generated` after adding or removing indexed files.

<!-- BEGIN FILE INDEX -->
## File Index

Autogenerated from first-line comments. Run scripts/update_agents_file_index.py to refresh.

- `detekt.yml`: Detekt configuration for caic Android project.
- `docs/DEBUGGING_EMULATOR.md`: Debugging with the Android Emulator
- `docs/HALO.md`: Halo Device Support
- `docs/WEB_SHELL.md`: Go Mode Android Web Shell Boundary
- `gomode/AGENTS.md`: Go Mode Android App
<!-- END FILE INDEX -->
