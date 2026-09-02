#!/usr/bin/env python3
"""Run Android E2E tests against the fake backend.

By default this runs all Android E2E modules. Use --module to run one module:
  python3 scripts/android_e2e.py --module gomode
  python3 scripts/android_e2e.py --module halo-sdk
  python3 scripts/android_e2e.py --module all

Steps:
  1. Build the fake backend (go build -tags e2e).
  2. Find a free port and start the backend on it.
  3. Wait until the backend responds.
  4. Run the selected connectedAndroidTest module(s) via Gradle, passing
     10.0.2.2:PORT (emulator's host alias — no adb reverse needed).
  5. With --screenshots, run only the Go Mode documentation generator and pull
     its images into the requested output directory.
  6. Dump logcat on failure for CI diagnostics.
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
GOMODE_DEVICE_SCREENSHOT_DIR = "/sdcard/Pictures/gomode-screenshots"
SCREENSHOT_TEST_CLASS = "com.fghbuild.gomode.GoModeDocumentationScreenshotsTest"
TEST_TASKS_BY_MODULE: dict[str, tuple[str, ...]] = {
    "gomode": (":gomode:connectedAndroidTest",),
    "halo-sdk": (":halo-sdk:connectedAndroidTest",),
}
TEST_TASKS_BY_MODULE["all"] = (
    *TEST_TASKS_BY_MODULE["gomode"],
    *TEST_TASKS_BY_MODULE["halo-sdk"],
)


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
    url = f"http://localhost:{port}/api/caic/v1/server/config"
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen(url, timeout=2)
            return True
        except urllib.error.HTTPError:
            return True
        except (TimeoutError, urllib.error.URLError):
            time.sleep(0.5)
    return False


def ready_adb_serials(output: str, requested_serial: str | None) -> list[str]:
    """Return ready adb serials, restricted to the requested serial when set."""
    serials = []
    for line in output.splitlines()[1:]:
        serial, separator, state = line.partition("\t")
        if separator and state.split(maxsplit=1)[0] == "device":
            serials.append(serial)
    if requested_serial is not None:
        return [serial for serial in serials if serial == requested_serial]
    return serials


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


APP_PACKAGES = (
    "com.fghbuild.gomode",
    "com.fghbuild.gomode.test",
)


def _running_app_pids():
    """Return currently running PIDs for Android app/test packages."""
    try:
        out = subprocess.run(
            ["adb", "shell", "pidof", *APP_PACKAGES],
            capture_output=True,
            text=True,
            timeout=5,
        )
        return out.stdout.strip().split()
    except subprocess.TimeoutExpired:
        return []


def _logged_app_pids(logcat_path):
    """Return PIDs seen in logcat lines that mention app/test packages."""
    pids = set()
    try:
        with open(logcat_path) as f:
            for line in f:
                if not any(package in line for package in APP_PACKAGES):
                    continue
                parts = line.split(maxsplit=5)
                if len(parts) >= 3 and parts[2].isdigit():
                    pids.add(parts[2])
    except OSError:
        return []
    return sorted(pids)


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
    # Prefer PIDs seen in the captured log because instrumentation may stop the
    # app before this failure handler runs.
    app_pids = sorted(set(_logged_app_pids(logcat_path)) | set(_running_app_pids()))
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


def persist_logcat_for_artifact(logcat_path, module):
    """Copy logcat to the failed module's reports directory for CI upload."""
    module_reports = {
        "gomode": os.path.join(ROOT_DIR, "android", "gomode", "build", "reports", "androidTests", "connected"),
        "halo-sdk": os.path.join(ROOT_DIR, "sdk", "halo", "build", "reports", "androidTests", "connected"),
    }
    dest_dir = module_reports.get(module, module_reports["gomode"])
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, "logcat.txt")
    shutil.copy2(logcat_path, dest)
    return dest


def is_emulator():
    """Return True if the connected device is an emulator."""
    result = subprocess.run(
        ["adb", "shell", "getprop", "ro.build.characteristics"],
        capture_output=True,
        text=True,
        timeout=5,
    )
    return "emulator" in result.stdout


def module_for_task(task):
    for module, tasks in TEST_TASKS_BY_MODULE.items():
        if module != "all" and task in tasks:
            return module
    return "gomode"


def normalize_visual_emulator():
    """Normalize rendering-affecting emulator state before screenshot instrumentation."""
    if not is_emulator():
        raise RuntimeError("documentation screenshots require the canonical Android emulator")
    commands = (
        ("settings", "put", "global", "animator_duration_scale", "0"),
        ("settings", "put", "global", "transition_animation_scale", "0"),
        ("settings", "put", "global", "window_animation_scale", "0"),
        ("settings", "put", "system", "font_scale", "1.0"),
        ("cmd", "uimode", "night", "no"),
        (
            "cmd",
            "statusbar",
            "send-disable-flag",
            "clock",
            "system-icons",
            "notification-icons",
        ),
        ("wm", "density", "reset"),
        ("wm", "size", "reset"),
    )
    for command in commands:
        subprocess.run(["adb", "shell", *command], check=True, capture_output=True)


def clear_visual_app_state():
    """Remove installed app packages so each visual pass starts from first launch."""
    for package in APP_PACKAGES:
        subprocess.run(["adb", "uninstall", package], capture_output=True, check=False)


def run_tests(port, module, screenshots):
    # 10.0.2.2 is the emulator's host loopback alias — no adb reverse needed.
    # Real devices need localhost + adb reverse.
    if is_emulator():
        host = "10.0.2.2"
    else:
        host = "localhost"
        subprocess.check_call(["adb", "reverse", f"tcp:{port}", f"tcp:{port}"])
    tasks = TEST_TASKS_BY_MODULE[module]
    if screenshots:
        subprocess.run(
            [
                "./gradlew",
                "--no-daemon",
                ":gomode:assembleDebug",
                ":gomode:assembleDebugAndroidTest",
            ],
            cwd=os.path.join(ROOT_DIR, "android"),
            check=True,
        )
        clear_visual_app_state()
        normalize_visual_emulator()

    for task in tasks:
        print(f"Running {task}...")
        command = [
            "./gradlew",
            "--no-daemon",
            task,
            f"-Pandroid.testInstrumentationRunnerArguments.baseUrl=http://{host}:{port}",
        ]
        if screenshots:
            command.extend(
                [
                    f"-Pandroid.testInstrumentationRunnerArguments.class={SCREENSHOT_TEST_CLASS}",
                    "-Pandroid.testInstrumentationRunnerArguments.caicVisualScreenshots=true",
                ],
            )
        result = subprocess.run(command, cwd=os.path.join(ROOT_DIR, "android"))
        if result.returncode != 0:
            return result.returncode, module_for_task(task)
    return 0, module


def clear_device_screenshots():
    """Remove stale screenshots whose directory may be owned by a prior app UID."""
    subprocess.run(
        ["adb", "shell", "rm", "-rf", GOMODE_DEVICE_SCREENSHOT_DIR],
        capture_output=True,
    )


def pull_screenshots(screenshot_dir):
    """Pull screenshots from device, convert to webp, clean up."""
    if shutil.which("ffmpeg") is None:
        print("ffmpeg is required to encode Android screenshots", file=sys.stderr)
        return 1

    result = subprocess.run(
        ["adb", "shell", "ls", f"{GOMODE_DEVICE_SCREENSHOT_DIR}/"],
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

    os.makedirs(screenshot_dir, exist_ok=True)
    for name in names:
        remote = f"{GOMODE_DEVICE_SCREENSHOT_DIR}/{name}.png"
        local_png = os.path.join(screenshot_dir, f"{name}.png")
        local_webp = os.path.join(screenshot_dir, f"{name}.webp")
        subprocess.run(["adb", "pull", remote, local_png], capture_output=True, check=True)
        subprocess.run(
            ["ffmpeg", "-y", "-i", local_png, "-lossless", "1", local_webp],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=True,
        )
        os.remove(local_png)
        print(f"  {name}.webp")

    subprocess.run(
        ["adb", "shell", "rm", "-rf", GOMODE_DEVICE_SCREENSHOT_DIR],
        capture_output=True,
    )
    return 0


def parse_args():
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--module",
        choices=tuple(TEST_TASKS_BY_MODULE.keys()),
        default="all",
        help="Android module to test. Defaults to all.",
    )
    parser.add_argument(
        "--screenshots",
        action="store_true",
        help="run only the Go Mode documentation screenshot generator",
    )
    parser.add_argument(
        "--screenshot-dir",
        help="output directory required with --screenshots",
    )
    parser.add_argument(
        "--port",
        type=int,
        help="fixed fake-backend port (visual workflows use this for stable displayed URLs)",
    )
    args = parser.parse_args()
    if args.screenshots and args.module != "gomode":
        parser.error("--screenshots requires --module gomode")
    if args.screenshots != (args.screenshot_dir is not None):
        parser.error("--screenshots and --screenshot-dir must be used together")
    return args


def main():
    args = parse_args()

    try:
        subprocess.run(["java", "-version"], capture_output=True, check=True)
    except (FileNotFoundError, subprocess.CalledProcessError, OSError):
        print(
            "Java is required. Install a JDK (17+) then run 'java -version' to verify:\n"
            "  macOS:  brew install openjdk@21\n"
            "          sudo ln -sfn $(brew --prefix)/opt/openjdk@21/libexec/openjdk.jdk \\\n"
            "            /Library/Java/JavaVirtualMachines/openjdk-21.jdk\n"
            "  Linux:  sudo apt install openjdk-21-jdk",
            file=sys.stderr,
        )
        return 1

    port = args.port if args.port is not None else find_free_port()
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
            requested_serial = os.environ.get("ANDROID_SERIAL")
            devices = ready_adb_serials(result.stdout, requested_serial)
            if len(devices) == 0:
                if requested_serial is not None:
                    print(
                        f"ANDROID_SERIAL device is not ready: {requested_serial}",
                        file=sys.stderr,
                    )
                else:
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
            if args.screenshots:
                clear_device_screenshots()
            print(f"Running Android E2E tests for {args.module}...")
            logcat_proc, logcat_path, logcat_file = start_logcat(tmp_dir)
            try:
                rc, failed_module = run_tests(port, args.module, args.screenshots)
            finally:
                stop_logcat(logcat_proc, logcat_file)

            if rc != 0:
                print(f"Tests failed (exit {rc}). Dumping logcat:", file=sys.stderr)
                dump_logcat_on_failure(logcat_path)
                persist_logcat_for_artifact(logcat_path, failed_module)
            elif args.screenshots:
                print("Pulling screenshots...")
                rc = pull_screenshots(args.screenshot_dir)
            else:
                print(f"Skipping screenshots for {args.module}.")

            return rc
        finally:
            proc.send_signal(signal.SIGTERM)
            proc.wait()
            log.close()
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
