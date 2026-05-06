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
import re
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
    config_path = os.path.join(tmp_dir, "config.toml")

    # Create a minimal config.toml with the dynamic port
    with open(config_path, "w") as f:
        f.write(f'[server]\nhttp = ":{port}"\n')

    log = open(log_path, "w")  # noqa: SIM115
    proc = subprocess.Popen(
        [binary, "-config-dir", tmp_dir],
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


def start_logcat(tmp_dir):
    """Start adb logcat in the background, writing to a temp file."""
    logcat_path = os.path.join(tmp_dir, "logcat.txt")
    logcat_file = open(logcat_path, "w")  # noqa: SIM115
    proc = subprocess.Popen(
        ["adb", "logcat", "-v", "threadtime"],
        stdout=logcat_file,
        stderr=logcat_file,
    )
    return proc, logcat_path, logcat_file


def stop_logcat(proc, logcat_file=None):
    """Kill the background logcat process and close the log file."""
    try:
        proc.terminate()
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
    if logcat_file:
        logcat_file.close()


def _get_app_pids():
    """Return PIDs for com.fghbuild.caic and com.fghbuild.caic.test, if running."""
    try:
        out = subprocess.run(
            ["adb", "shell", "pidof", "com.fghbuild.caic", "com.fghbuild.caic.test"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        return out.stdout.strip().split()
    except subprocess.TimeoutExpired:
        return []


def dump_logcat_on_failure(logcat_path):
    """Dump logcat to stderr for immediate CI visibility."""
    tail_lines = 500
    print(f"--- LOGCAT (last {tail_lines} lines) ---", file=sys.stderr)
    try:
        with open(logcat_path) as f:
            lines = f.readlines()
            for line in lines[-tail_lines:]:
                print(line.rstrip(), file=sys.stderr)
    except OSError as e:
        print(f"Failed to read logcat: {e}", file=sys.stderr)
    print("--- END LOGCAT ---", file=sys.stderr)

    # Dump only lines belonging to our app processes. Logcat threadtime format
    # includes the PID as the first column after the date: "05-06 17:58:10.123  4395".
    # We match by PID so this stays correct as tags are added, renamed, or removed.
    app_pids = _get_app_pids()
    app_tail = 300
    print(
        f"--- APP LOGCAT (last {app_tail} lines for PIDs {app_pids}) ---",
        file=sys.stderr,
    )
    if app_pids:
        try:
            with open(logcat_path) as f:
                lines = [line for line in f if any(f" {pid} " in line for pid in app_pids)]
                for line in lines[-app_tail:]:
                    print(line.rstrip(), file=sys.stderr)
        except OSError as e:
            print(f"Failed to read logcat: {e}", file=sys.stderr)
    else:
        print("(app not running — no PIDs to filter by)", file=sys.stderr)
    print("--- END APP LOGCAT ---", file=sys.stderr)


def persist_logcat_for_artifact(logcat_path):
    """Copy logcat to android/app/build/reports/ so CI can upload it as an artifact."""
    dest_dir = os.path.join(ROOT_DIR, "android", "app", "build", "reports", "androidTests", "connected")
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, "logcat.txt")
    shutil.copy2(logcat_path, dest)
    return dest


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
        print(
            "WARNING: ffmpeg not found; screenshots will be kept as PNG",
            file=sys.stderr,
        )

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

    def image_identity(a: str, b: str) -> float:
        """Return identity score (1.0 = identical) between two images."""
        r = subprocess.run(
            ["ffmpeg", "-i", a, "-i", b, "-lavfi", "identity", "-f", "null", "-"],
            capture_output=True,
            text=True,
        )
        m = re.search(r"average:([\d.]+)", r.stderr)
        return float(m.group(1)) if m else 0.0

    os.makedirs(SCREENSHOT_DIR, exist_ok=True)
    for name in names:
        remote = f"{device_dir}/{name}.png"
        local_png = os.path.join(SCREENSHOT_DIR, f"{name}.png")
        local_webp = os.path.join(SCREENSHOT_DIR, f"{name}.webp")
        subprocess.run(["adb", "pull", remote, local_png], capture_output=True)
        if has_ffmpeg:
            # Lossless encoding preserves pixels exactly, so we can compare
            # the source PNG against the existing webp without a temp file.
            if os.path.exists(local_webp):
                score = image_identity(local_png, local_webp)
                pct = (1 - score) * 100
                if pct < 1:
                    os.remove(local_png)
                    print(f"  {name}.webp ({pct:.1f}% different, keeping)")
                    continue
                print(f"  {name}.webp ({pct:.1f}% different, updating)")
            else:
                print(f"  {name}.webp (new)")
            subprocess.check_call(
                ["ffmpeg", "-y", "-i", local_png, "-lossless", "1", local_webp],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            os.remove(local_png)
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
                print("No adb devices found.", file=sys.stderr)
                print("Start one with:  make android-start-emulator", file=sys.stderr)
                print("Stop it with:    make android-stop-emulator", file=sys.stderr)
                return 1
            if len(devices) > 1:
                print(
                    f"Multiple adb devices found ({len(devices)}). Use ANDROID_SERIAL to select one.",
                    file=sys.stderr,
                )
                return 1
            subprocess.check_call(["adb", "reverse", f"tcp:{port}", f"tcp:{port}"])

            print("Running Android E2E tests...")
            logcat_proc, logcat_path, logcat_file = start_logcat(tmp_dir)
            try:
                rc = run_tests(port)
            finally:
                stop_logcat(logcat_proc, logcat_file)

            if rc != 0:
                print(f"Tests failed (exit {rc}). Dumping logcat:", file=sys.stderr)
                dump_logcat_on_failure(logcat_path)
                persist_logcat_for_artifact(logcat_path)
            else:
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
