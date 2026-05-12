#!/usr/bin/env python3
"""Start the caic_test Android emulator and wait until adb can see it.

Requires a completed "make android-setup-emulator" first.
Works on Linux and macOS.
"""

import argparse
import os
import platform
import shutil
import subprocess
import sys
import time

AVD_NAME = "caic_test"

EMULATOR_ARGS = [
    "-no-window",
    "-no-audio",
    "-gpu",
    "swiftshader_indirect",
    "-no-boot-anim",
    "-wipe-data",
]


def _sdk_root() -> str:
    """Return the Android SDK root from environment or default location."""
    for var in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        val = os.environ.get(var)
        if val and os.path.isdir(val):
            return val

    default = os.path.expanduser("~/Android/Sdk")
    if os.path.isdir(default):
        return default

    sys.exit("Could not find Android SDK. Set ANDROID_HOME or run:\n  make android-setup-emulator")


def _find_tool(name: str, sdk_root: str) -> str:
    """Return the path to a tool, checking PATH then SDK directories."""
    path = shutil.which(name)
    if path:
        return path

    # Check SDK subdirectories.
    for subdir in ("emulator", "platform-tools", "cmdline-tools/latest/bin"):
        candidate = os.path.join(sdk_root, subdir, name)
        if os.path.isfile(candidate):
            return candidate

    sys.exit(f"Could not find '{name}'. Run 'make android-setup-emulator' first.\nSearched PATH and {sdk_root}")


def _emulator_exited(proc: subprocess.Popen) -> str | None:
    """Return an error message if the emulator process exited, or None."""
    rc = proc.poll()
    if rc is None:
        return None

    # Process exited — read any buffered stderr.
    stderr = b""
    if proc.stderr:
        try:
            stderr = proc.stderr.read()
        except OSError:
            pass

    detail = ""
    if stderr:
        detail = f"\n{stderr.decode(errors='replace').strip()}"
    return f"Emulator exited with code {rc} before adb could connect.{detail}"


def _wait_for_device(adb: str, emulator_proc: subprocess.Popen, timeout: int = 180) -> None:
    """Wait for an adb device to become ready and finish booting.

    Also monitors the emulator process so we can fail fast if it exits
    early instead of waiting for the full adb timeout.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        # Check if the emulator died.
        msg = _emulator_exited(emulator_proc)
        if msg:
            sys.exit(msg)

        # Poll adb with a short timeout so we can re-check the emulator.
        try:
            subprocess.run(
                [adb, "wait-for-device"],
                capture_output=True,
                timeout=min(5, deadline - time.monotonic()),
            )
            # Connected — now wait for boot.
            _wait_for_boot(adb, emulator_proc, deadline)
            return
        except subprocess.TimeoutExpired:
            continue

    sys.exit(f"No adb device connected after {timeout}s.")


def _wait_for_boot(adb: str, emulator_proc: subprocess.Popen, deadline: float) -> None:
    """Wait until the device's boot animation completes."""
    while time.monotonic() < deadline:
        msg = _emulator_exited(emulator_proc)
        if msg:
            sys.exit(msg)

        try:
            result = subprocess.run(
                [adb, "shell", "getprop", "sys.boot_completed"],
                capture_output=True,
                text=True,
                timeout=10,
            )
            if result.stdout.strip() == "1":
                return
        except subprocess.TimeoutExpired:
            pass
        time.sleep(2)

    sys.exit("Device did not finish booting in time.")


def _check_host() -> None:
    """Refuse to start the emulator on unsupported hosts."""
    if platform.system() == "Linux" and platform.machine() == "aarch64":
        sys.exit(
            "The Android emulator is not available for ARM64 Linux.\nUse an x86_64 host, a physical device, or macOS."
        )


def main() -> int:
    argparse.ArgumentParser(description=__doc__).parse_args()

    _check_host()

    sdk = _sdk_root()
    emulator = _find_tool("emulator", sdk)
    adb = _find_tool("adb", sdk)

    print(f"Starting emulator '{AVD_NAME}'...", file=sys.stderr)
    proc = subprocess.Popen(
        [emulator, "-avd", AVD_NAME, *EMULATOR_ARGS],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )

    print("Waiting for device...", file=sys.stderr)
    try:
        _wait_for_device(adb, proc)
    except SystemExit:
        proc.kill()
        proc.wait()
        raise

    print("Emulator is ready.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
