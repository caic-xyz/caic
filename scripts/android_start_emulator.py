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
import tempfile
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


def _sdk_root() -> str | None:
    """Return the Android SDK root from environment or default location."""
    for var in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        val = os.environ.get(var)
        if val and os.path.isdir(val):
            return val

    default = os.path.expanduser("~/Android/Sdk")
    if os.path.isdir(default):
        return default

    print("Could not find Android SDK. Set ANDROID_HOME or run:\n  make android-setup-emulator", file=sys.stderr)
    return None


def _find_tool(name: str, sdk_root: str) -> str | None:
    """Return the path to a tool, checking PATH then SDK directories."""
    path = shutil.which(name)
    if path:
        return path

    # Check SDK subdirectories.
    for subdir in ("emulator", "platform-tools", "cmdline-tools/latest/bin"):
        candidate = os.path.join(sdk_root, subdir, name)
        if os.path.isfile(candidate):
            return candidate

    print(
        f"Could not find '{name}'. Run 'make android-setup-emulator' first.\nSearched PATH and {sdk_root}",
        file=sys.stderr,
    )
    return None


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


def _wait_for_device(adb: str, emulator_proc: subprocess.Popen, timeout: int = 180) -> int:
    """Wait for an adb device to become ready and finish booting.

    Also monitors the emulator process so we can fail fast if it exits
    early instead of waiting for the full adb timeout.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        # Check if the emulator died.
        msg = _emulator_exited(emulator_proc)
        if msg:
            print(msg, file=sys.stderr)
            return 1

        # Poll adb with a short timeout so we can re-check the emulator.
        try:
            subprocess.run(
                [adb, "wait-for-device"],
                capture_output=True,
                timeout=min(5, deadline - time.monotonic()),
            )
            # Connected — now wait for boot.
            return _wait_for_boot(adb, emulator_proc, deadline)
        except subprocess.TimeoutExpired:
            continue

    print(f"No adb device connected after {timeout}s.", file=sys.stderr)
    return 1


def _wait_for_boot(adb: str, emulator_proc: subprocess.Popen, deadline: float) -> int:
    """Wait until the device's boot animation completes."""
    while time.monotonic() < deadline:
        msg = _emulator_exited(emulator_proc)
        if msg:
            print(msg, file=sys.stderr)
            return 1

        try:
            result = subprocess.run(
                [adb, "shell", "getprop", "sys.boot_completed"],
                capture_output=True,
                text=True,
                timeout=10,
            )
            if result.stdout.strip() == "1":
                return 0
        except subprocess.TimeoutExpired:
            pass
        time.sleep(2)

    print("Device did not finish booting in time.", file=sys.stderr)
    return 1


def _check_host() -> int:
    """Refuse to start the emulator on unsupported hosts."""
    if platform.system() == "Linux" and platform.machine() == "aarch64":
        print(
            "The Android emulator is not available for ARM64 Linux.\nUse an x86_64 host, a physical device, or macOS.",
            file=sys.stderr,
        )
        return 1
    return 0


def main() -> int:
    argparse.ArgumentParser(description=__doc__).parse_args()

    if _check_host() != 0:
        return 1

    sdk = _sdk_root()
    if sdk is None:
        return 1
    emulator = _find_tool("emulator", sdk)
    if emulator is None:
        return 1
    adb = _find_tool("adb", sdk)
    if adb is None:
        return 1
    log_path = os.path.join(tempfile.gettempdir(), f"{AVD_NAME}-emulator.log")
    log = open(log_path, "w")  # noqa: SIM115

    print(f"Starting emulator '{AVD_NAME}'...", file=sys.stderr)
    proc = subprocess.Popen(
        [emulator, "-avd", AVD_NAME, *EMULATOR_ARGS],
        stdout=subprocess.DEVNULL,
        stderr=log,
        start_new_session=True,
    )

    print("Waiting for device...", file=sys.stderr)
    try:
        wait_status = _wait_for_device(adb, proc)
        if wait_status != 0:
            proc.kill()
            proc.wait()
            return wait_status
    except BaseException:
        proc.kill()
        proc.wait()
        raise
    finally:
        log.close()

    print("Emulator is ready.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
