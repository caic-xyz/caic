#!/usr/bin/env python3
"""Start or reuse the caic_test Android emulator and wait until it has booted.

The make target runs SDK and AVD setup before starting this script.
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
DEFAULT_SDK_ROOT = os.path.expanduser("~/.local/share/android-sdk")

EMULATOR_ARGS = [
    "-no-window",
    "-no-audio",
    "-gpu",
    "swiftshader_indirect",
    "-change-locale",
    "en-US",
    "-dpi-device",
    "420",
    "-no-boot-anim",
    "-no-snapstorage",
    "-timezone",
    "UTC",
    "-wipe-data",
    "-memory",
    "2048",
    "-partition-size",
    "4096",
]


def _sdk_root() -> str | None:
    """Return the Android SDK root from common local and CI locations."""
    roots: list[str] = []
    for var in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        value = os.environ.get(var)
        if value and value not in roots:
            roots.append(value)
    for root in (
        DEFAULT_SDK_ROOT,
        os.path.expanduser("~/Android/Sdk"),
        os.path.expanduser("~/Library/Android/sdk"),
        "/usr/local/lib/android/sdk",
    ):
        if root not in roots:
            roots.append(root)
    for root in roots:
        if os.path.isdir(root):
            return root
    print("Could not find Android SDK. Set ANDROID_HOME or run:\n  make android-start-emulator", file=sys.stderr)
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
        f"Could not find '{name}'. Run 'make android-start-emulator' first.\nSearched PATH and {sdk_root}",
        file=sys.stderr,
    )
    return None


def _running_avd_serial(adb: str) -> str | None:
    """Return the serial of the running caic_test AVD, if present."""
    devices = subprocess.run([adb, "devices"], capture_output=True, check=True, text=True)
    for line in devices.stdout.splitlines()[1:]:
        serial, separator, state = line.partition("\t")
        if separator == "" or state != "device" or not serial.startswith("emulator-"):
            continue
        avd = subprocess.run(
            [adb, "-s", serial, "emu", "avd", "name"],
            capture_output=True,
            check=True,
            text=True,
        )
        if AVD_NAME in (line.strip() for line in avd.stdout.splitlines()):
            return serial
    return None


def _emulator_exited(proc: subprocess.Popen, log_path: str) -> str | None:
    """Return an error message if the emulator process exited, or None."""
    rc = proc.poll()
    if rc is None:
        return None

    log_tail = ""
    try:
        with open(log_path) as f:
            lines = f.readlines()
            if lines:
                log_tail = "\n" + "".join(lines[-40:])
    except OSError:
        pass

    return f"Emulator exited with code {rc} before adb could connect.{log_tail}"


def _wait_for_device(adb: str, emulator_proc: subprocess.Popen, log_path: str, timeout: int = 180) -> int:
    """Wait for an adb device to become ready and finish booting.

    Also monitors the emulator process so we can fail fast if it exits
    early instead of waiting for the full adb timeout.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        # Check if the emulator died.
        msg = _emulator_exited(emulator_proc, log_path)
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
            return _wait_for_boot(adb, emulator_proc, log_path, deadline)
        except subprocess.TimeoutExpired:
            continue

    print(f"No adb device connected after {timeout}s.", file=sys.stderr)
    return 1


def _wait_for_boot(adb: str, emulator_proc: subprocess.Popen, log_path: str, deadline: float) -> int:
    """Wait until the device's boot animation completes."""
    while time.monotonic() < deadline:
        msg = _emulator_exited(emulator_proc, log_path)
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


def _wait_for_existing_boot(adb: str, serial: str, timeout: int = 180) -> int:
    """Wait for an already-running emulator to finish booting."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                [adb, "-s", serial, "shell", "getprop", "sys.boot_completed"],
                capture_output=True,
                text=True,
                timeout=10,
            )
            if result.stdout.strip() == "1":
                return 0
        except subprocess.TimeoutExpired:
            pass
        time.sleep(2)

    print(f"Emulator {serial} did not finish booting in time.", file=sys.stderr)
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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--auto-reuse",
        action="store_true",
        help="reuse a running caic_test emulator instead of starting another one",
    )
    args = parser.parse_args()

    if _check_host() != 0:
        return 1

    sdk = _sdk_root()
    if sdk is None:
        return 1
    adb = _find_tool("adb", sdk)
    if adb is None:
        return 1
    if args.auto_reuse:
        serial = _running_avd_serial(adb)
        if serial is not None:
            print(f"Reusing emulator '{AVD_NAME}' ({serial})...", file=sys.stderr)
            return _wait_for_existing_boot(adb, serial)

    emulator = _find_tool("emulator", sdk)
    if emulator is None:
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
        wait_status = _wait_for_device(adb, proc, log_path)
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

    print(f"Emulator is ready (log: {log_path}).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
