#!/usr/bin/env python3
"""Run Android E2E tests against the fake backend.

Steps:
  1. Build the fake backend (go build -tags e2e).
  2. Find a free port and start the backend on it.
  3. Wait until the backend responds.
  4. Set up adb reverse port forwarding.
  5. Run connectedAndroidTest via Gradle with the dynamic port.
  6. Pull and convert screenshots.
  7. Kill the backend on exit.
"""

import argparse
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCREENSHOT_DIR = os.path.join(ROOT_DIR, "e2e", "screenshots", "android")


def find_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("", 0))
        return s.getsockname()[1]


def build_backend(tmp_dir):
    binary = os.path.join(tmp_dir, "caic-e2e")
    subprocess.check_call(
        ["go", "build", "-tags", "e2e", "-o", binary, "./backend/cmd/caic"],
        cwd=ROOT_DIR,
    )
    return binary


def start_backend(tmp_dir, binary, port):
    log_path = os.path.join(tmp_dir, "caic-e2e.log")
    log = open(log_path, "w")  # noqa: SIM115
    proc = subprocess.Popen(
        [binary, "-http", f":{port}"],
        stdout=log,
        stderr=log,
    )
    return proc, log, log_path


def wait_for_backend(port):
    url = f"http://localhost:{port}/api/v1/server/config"
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen(url, timeout=2)
            return True
        except urllib.error.HTTPError:
            return True
        except urllib.error.URLError:
            time.sleep(0.5)
    return False


def run_tests(port):
    result = subprocess.run(
        [
            "./gradlew",
            "--no-daemon",
            "connectedAndroidTest",
            f"-Pandroid.testInstrumentationRunnerArguments.baseUrl=http://localhost:{port}",
        ],
        cwd=os.path.join(ROOT_DIR, "android"),
    )
    return result.returncode


def pull_screenshots():
    """Pull screenshots from device, convert to webp, clean up."""
    has_ffmpeg = shutil.which("ffmpeg") is not None
    if not has_ffmpeg:
        print("WARNING: ffmpeg not found; screenshots will be kept as PNG", file=sys.stderr)

    device_dir = "/sdcard/Pictures/caic-screenshots"
    result = subprocess.run(
        ["adb", "shell", "ls", f"{device_dir}/"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print("No screenshots found on device", file=sys.stderr)
        return 1

    names = [f.removesuffix(".png") for f in result.stdout.split() if f.endswith(".png")]
    if not names:
        print("No screenshots found on device", file=sys.stderr)
        return 1

    os.makedirs(SCREENSHOT_DIR, exist_ok=True)
    for name in names:
        remote = f"{device_dir}/{name}.png"
        local_png = os.path.join(SCREENSHOT_DIR, f"{name}.png")
        local_webp = os.path.join(SCREENSHOT_DIR, f"{name}.webp")
        subprocess.run(["adb", "pull", remote, local_png], capture_output=True)
        if has_ffmpeg:
            subprocess.check_call(
                ["ffmpeg", "-y", "-i", local_png, "-lossless", "1", local_webp],
                capture_output=True,
            )
            if os.path.exists(local_png):
                os.remove(local_png)
            print(f"  {name}.webp")
        else:
            print(f"  {name}.png")

    subprocess.run(
        ["adb", "shell", "rm", "-rf", device_dir],
        capture_output=True,
    )
    return 0


def main():
    argparse.ArgumentParser(description=__doc__).parse_args()

    port = find_free_port()
    tmp_dir = tempfile.mkdtemp(prefix="caic-e2e-")
    try:
        print("Building fake backend...")
        binary = build_backend(tmp_dir)

        proc, log, log_path = start_backend(tmp_dir, binary, port)
        try:
            print(f"Waiting for fake backend on :{port}...")
            if not wait_for_backend(port):
                print("Fake backend failed to start; log:", file=sys.stderr)
                log.close()
                with open(log_path) as f:
                    print(f.read(), file=sys.stderr)
                return 1

            result = subprocess.run(
                ["adb", "devices"],
                capture_output=True,
                text=True,
                check=True,
            )
            devices = [line for line in result.stdout.strip().splitlines()[1:] if line.strip()]
            if len(devices) == 0:
                print("No adb devices found. Start an emulator or connect a device.", file=sys.stderr)
                return 1
            if len(devices) > 1:
                print(
                    f"Multiple adb devices found ({len(devices)}). Use ANDROID_SERIAL to select one.",
                    file=sys.stderr,
                )
                return 1
            subprocess.check_call(["adb", "reverse", f"tcp:{port}", f"tcp:{port}"])

            print("Running Android E2E tests...")
            rc = run_tests(port)

            if rc == 0:
                print("Pulling screenshots...")
                rc = pull_screenshots()

            return rc
        finally:
            proc.send_signal(signal.SIGTERM)
            proc.wait()
            log.close()
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
