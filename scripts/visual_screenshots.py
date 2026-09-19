#!/usr/bin/env python3
"""Render, check, or update deterministic frontend and Android documentation screenshots.

Baseline comparison is a maintainer check: the tracked images encode the
development container's font stack, so comparing renders from a different host
reports typeface differences that are not product regressions. The generate mode
renders without comparing, which is what CI gates so a stale generator cannot
rot silently."""

import argparse
import concurrent.futures
import os
import platform
import re
import shutil
import socket
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

from android_e2e import ready_adb_serials
from android_sdk import AVD_NAME, EMULATOR_API, EMULATOR_TAG, find_sdkmanager

ROOT_DIR = Path(__file__).resolve().parent.parent
ANDROID_VISUAL_PORT = 41743
FRONTEND_VISUAL_PORTS = (41741, 41742)
FRONTEND_VISUAL_HOST = "127.0.0.1"
BASELINE_DIRS = {
    "android": ROOT_DIR / "e2e" / "screenshots" / "android",
    "frontend": ROOT_DIR / "e2e" / "screenshots" / "frontend",
}
IMAGE_SUFFIXES = frozenset({".avif", ".png", ".webp"})
VISUAL_SEED = "caic-visual-v1"
# Playwright deletes test-results/ at the start of every run, so failed renders
# live beside it instead of inside it: they have to survive a re-run to be
# inspected, and CI uploads this directory when a check fails.
FAILURE_DIR = ROOT_DIR / "visual-screenshots-failures"
MAX_LUMA_DELTA = 2
MAX_LUMA_ERROR_PER_MILLION_PIXELS = 20
ANDROID_SYSTEM_IMAGE_REVISION = "9"
ANDROID_WEBVIEW_PACKAGE = "com.google.android.webview"
ANDROID_WEBVIEW_VERSION = "124.0.6367.219"
FRONTEND_SPECS = (
    "e2e/tests/gen-screenshots.spec.ts",
    "e2e/tests/prompt-input.spec.ts",
)


@dataclass(frozen=True)
class LumaDifference:
    """Absolute decoded-luma differences between two image sequences."""

    maximum: int
    pixel_count: int
    total: int

    @property
    def allowed_total(self) -> int:
        return (self.pixel_count * MAX_LUMA_ERROR_PER_MILLION_PIXELS + 999_999) // 1_000_000

    @property
    def acceptable(self) -> bool:
        return self.maximum <= MAX_LUMA_DELTA and self.total <= self.allowed_total


def parse_properties(contents: str) -> dict[str, str]:
    """Parse non-commented key/value lines from an Android properties file."""
    properties: dict[str, str] = {}
    for line in contents.splitlines():
        key, separator, value = line.partition("=")
        if separator and not key.lstrip().startswith("#"):
            properties[key.strip()] = value.strip()
    return properties


def android_system_image_path(sdk_root: Path) -> Path:
    """Return the canonical system-image directory for this host architecture."""
    return sdk_root / "system-images" / EMULATOR_API / EMULATOR_TAG / canonical_android_abi()


def canonical_android_abi() -> str:
    """Return the ABI of the canonical emulator image for this host."""
    return "arm64-v8a" if platform.machine() in ("aarch64", "arm64") else "x86_64"


def android_avd_config_path() -> Path:
    """Return the selected caic_test AVD's configuration path."""
    default_home = Path(os.environ.get("ANDROID_SDK_HOME", Path.home() / ".android")) / "avd"
    avd_home = Path(os.environ.get("ANDROID_AVD_HOME", default_home)).expanduser()
    return avd_home / f"{AVD_NAME}.avd" / "config.ini"


def current_webview_provider(details: str) -> tuple[str, str] | None:
    """Return the active WebView provider package and version."""
    match = re.search(r"Current WebView package \(name, version\): \(([^,]+), ([^)]+)\)", details)
    if match is None:
        return None
    return match.group(1), match.group(2)


def android_sdk_root() -> Path:
    """Return the SDK root containing the canonical system image."""
    sdk = find_sdkmanager()
    if sdk is None:
        raise RuntimeError("Android SDK not found. Run 'make android-setup-emulator' and retry.")
    sdk_root = Path(sdk[1]).resolve()
    if not android_system_image_path(sdk_root).is_dir():
        raise RuntimeError(
            f"Canonical Android screenshot system image not found under {sdk_root}. "
            "Run 'make android-setup-emulator' and retry.",
        )
    return sdk_root


def adb_output(adb: str, serial: str, *arguments: str) -> str:
    """Run one adb command against serial and return stripped stdout."""
    result = subprocess.run(
        [adb, "-s", serial, *arguments],
        capture_output=True,
        check=True,
        text=True,
    )
    return result.stdout.strip()


def verify_android_visual_environment() -> None:
    """Verify the rendering-sensitive Android image and WebView versions."""
    sdk_root = android_sdk_root()
    system_image = android_system_image_path(sdk_root)
    properties_path = system_image / "source.properties"
    if not properties_path.is_file():
        raise RuntimeError(f"Android system-image metadata is missing: {properties_path}")
    revision = parse_properties(properties_path.read_text()).get("Pkg.Revision")
    if revision != ANDROID_SYSTEM_IMAGE_REVISION:
        raise RuntimeError(
            "Android screenshot system-image revision mismatch: "
            f"expected {ANDROID_SYSTEM_IMAGE_REVISION}, found {revision or 'missing'} at {properties_path}. "
            "Restore the canonical SDK image before updating visual baselines.",
        )
    build_properties_path = system_image / "build.prop"
    if not build_properties_path.is_file():
        raise RuntimeError(f"Android system-image build metadata is missing: {build_properties_path}")
    installed_fingerprint = parse_properties(build_properties_path.read_text()).get("ro.system.build.fingerprint")
    if not installed_fingerprint:
        raise RuntimeError(f"Android system-image fingerprint is missing from {build_properties_path}")

    avd_config_path = android_avd_config_path()
    if not avd_config_path.is_file():
        raise RuntimeError(f"Canonical Android AVD configuration is missing: {avd_config_path}")
    avd_config = parse_properties(avd_config_path.read_text())
    expected_abi = canonical_android_abi()
    expected_avd_config = {
        "abi.type": expected_abi,
        "tag.id": EMULATOR_TAG,
        "target": EMULATOR_API,
    }
    mismatches = [
        f"{key}={avd_config.get(key, 'missing')} (expected {expected})"
        for key, expected in expected_avd_config.items()
        if avd_config.get(key) != expected
    ]
    if mismatches:
        raise RuntimeError(
            f"Android AVD {AVD_NAME} uses the wrong system image: {', '.join(mismatches)}. "
            "Stop the emulator, run 'make android-setup-emulator', and restart it.",
        )

    adb = shutil.which("adb")
    if adb is None:
        raise RuntimeError("adb is required to verify the Android screenshot environment")
    devices = subprocess.run([adb, "devices"], capture_output=True, check=True, text=True)
    requested_serial = os.environ.get("ANDROID_SERIAL")
    serials = ready_adb_serials(devices.stdout, requested_serial)
    if not serials:
        selection = f"ANDROID_SERIAL device {requested_serial!r}" if requested_serial else "Android emulator"
        raise RuntimeError(f"{selection} is not ready. Run 'make android-start-emulator' and retry.")
    if len(serials) > 1:
        raise RuntimeError("Multiple adb devices are ready. Set ANDROID_SERIAL to the canonical caic_test emulator.")
    serial = serials[0]
    avd_lines = [line for line in adb_output(adb, serial, "emu", "avd", "name").splitlines() if line != "OK"]
    avd_name = avd_lines[0] if avd_lines else "unknown"
    if avd_name != AVD_NAME:
        raise RuntimeError(
            f"Android screenshots require the {AVD_NAME} emulator, but {serial} runs {avd_name!r}.",
        )
    runtime_sdk = adb_output(adb, serial, "shell", "getprop", "ro.build.version.sdk")
    runtime_abi = adb_output(adb, serial, "shell", "getprop", "ro.product.cpu.abi")
    fingerprint = adb_output(adb, serial, "shell", "getprop", "ro.build.fingerprint")
    runtime_mismatches = []
    if runtime_sdk != EMULATOR_API.removeprefix("android-"):
        runtime_mismatches.append(f"API {runtime_sdk or 'missing'}")
    if runtime_abi != expected_abi:
        runtime_mismatches.append(f"ABI {runtime_abi or 'missing'}")
    if fingerprint != installed_fingerprint:
        runtime_mismatches.append(
            f"fingerprint {fingerprint or 'missing'} (expected installed image {installed_fingerprint})",
        )
    if runtime_mismatches:
        raise RuntimeError(
            f"Running emulator {serial} does not match its canonical Google APIs image: "
            f"{', '.join(runtime_mismatches)}. Stop it and run 'make android-start-emulator'.",
        )

    webview_details = adb_output(adb, serial, "shell", "dumpsys", "webviewupdate")
    provider = current_webview_provider(webview_details)
    if provider != (ANDROID_WEBVIEW_PACKAGE, ANDROID_WEBVIEW_VERSION):
        actual = f"{provider[0]} {provider[1]}" if provider is not None else "missing"
        raise RuntimeError(
            "Android active WebView provider mismatch: "
            f"expected {ANDROID_WEBVIEW_PACKAGE} {ANDROID_WEBVIEW_VERSION}, found {actual} on {serial}. "
            "Stop and restart the canonical emulator to wipe updates, then retry.",
        )


def image_files(directory: Path) -> dict[str, Path]:
    """Return generated image files keyed by their relative POSIX path."""
    if not directory.is_dir():
        return {}
    return {
        path.relative_to(directory).as_posix(): path
        for path in sorted(directory.rglob("*"))
        if path.is_file() and path.suffix in IMAGE_SUFFIXES
    }


def video_layout(path: Path) -> str:
    """Return the ordered decoded-video layout reported by ffprobe."""
    result = subprocess.run(
        [
            "ffprobe",
            "-v",
            "error",
            "-select_streams",
            "v",
            "-show_entries",
            "stream=height,nb_frames,width",
            "-of",
            "csv=p=0",
            "-i",
            str(path),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout


def decoded_luma(path: Path) -> bytes:
    """Decode every frame to full-range BT.709 8-bit luminance."""
    result = subprocess.run(
        [
            "ffmpeg",
            "-v",
            "error",
            "-i",
            str(path),
            "-map",
            "0:v",
            "-vf",
            ("format=rgba,scale=in_color_matrix=bt709:out_color_matrix=bt709:in_range=full:out_range=full,format=gray"),
            "-pix_fmt",
            "gray",
            "-f",
            "rawvideo",
            "-",
        ],
        check=True,
        capture_output=True,
    )
    return result.stdout


def luma_difference(actual: bytes, expected: bytes) -> LumaDifference:
    """Measure absolute differences between equal-length decoded luma planes."""
    if len(actual) != len(expected):
        raise ValueError("decoded luma planes have different lengths")
    maximum = 0
    total = 0
    for actual_value, expected_value in zip(actual, expected, strict=True):
        delta = abs(actual_value - expected_value)
        maximum = max(maximum, delta)
        total += delta
    return LumaDifference(maximum=maximum, pixel_count=len(actual), total=total)


def compare_images(actual_dir: Path, expected_dir: Path, label: str) -> list[str]:
    """Return file-set, layout, and bounded decoded-luma differences."""
    actual = image_files(actual_dir)
    expected = image_files(expected_dir)
    differences: list[str] = []
    for name in sorted(actual.keys() - expected.keys()):
        differences.append(f"{label}: unexpected image {name}")
    for name in sorted(expected.keys() - actual.keys()):
        differences.append(f"{label}: missing image {name}")
    for name in sorted(actual.keys() & expected.keys()):
        actual_layout = video_layout(actual[name])
        expected_layout = video_layout(expected[name])
        if actual_layout != expected_layout:
            differences.append(f"{label}: video layout differs for {name}")
            continue
        difference = luma_difference(
            decoded_luma(actual[name]),
            decoded_luma(expected[name]),
        )
        if not difference.acceptable:
            differences.append(
                f"{label}: luminance differs for {name} "
                f"(maximum {difference.maximum}/{MAX_LUMA_DELTA}, "
                f"total {difference.total}/{difference.allowed_total})",
            )
    return differences


def require_available_loopback_ports(ports: tuple[int, ...]) -> None:
    """Fail clearly if a reserved frontend visual-test port is unavailable."""
    listeners: list[socket.socket] = []
    try:
        for port in ports:
            listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                listener.bind((FRONTEND_VISUAL_HOST, port))
            except OSError as exc:
                listener.close()
                raise RuntimeError(
                    f"Frontend screenshot port {port} is unavailable. Stop the process using it and retry.",
                ) from exc
            listeners.append(listener)
    finally:
        for listener in listeners:
            listener.close()


def run_frontend_spec(command: list[str], env: dict[str, str]) -> None:
    """Run a frontend visual spec quietly, replaying full output on failure."""
    result = subprocess.run(
        command,
        cwd=ROOT_DIR,
        env=env,
        capture_output=True,
        text=True,
    )
    if result.returncode == 0:
        return
    if result.stdout:
        print(result.stdout, file=sys.stderr, end="" if result.stdout.endswith("\n") else "\n")
    if result.stderr:
        print(result.stderr, file=sys.stderr, end="" if result.stderr.endswith("\n") else "\n")
    result.check_returncode()


def render_frontend(output_dir: Path, port: int = FRONTEND_VISUAL_PORTS[0]) -> None:
    """Render the frontend visual tests into output_dir."""
    output_dir.mkdir(parents=True)
    env = os.environ.copy()
    env.update(
        {
            "CAIC_E2E_OUTPUT_DIR": str(FAILURE_DIR / "playwright" / output_dir.parent.name / output_dir.name),
            "CAIC_E2E_HOST": FRONTEND_VISUAL_HOST,
            "CAIC_E2E_PORT": str(port),
            "CAIC_E2E_SEED": VISUAL_SEED,
            "CAIC_E2E_VISUALS": "1",
            "CAIC_SCREENSHOT_DIR": str(output_dir),
            "CI": "",
            "LC_ALL": "C.UTF-8",
            "TZ": "UTC",
        },
    )
    for spec in FRONTEND_SPECS:
        run_frontend_spec(
            [
                "pnpm",
                "exec",
                "playwright",
                "test",
                "--config",
                "e2e/playwright.config.ts",
                spec,
                "--workers=1",
            ],
            env,
        )


def render_frontend_passes(first: Path, second: Path) -> None:
    """Render both isolated frontend passes concurrently."""
    require_available_loopback_ports(FRONTEND_VISUAL_PORTS)
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        futures = [
            executor.submit(render_frontend, output, port)
            for output, port in zip((first, second), FRONTEND_VISUAL_PORTS, strict=True)
        ]
        for future in concurrent.futures.as_completed(futures):
            future.result()


def render_android(output_dir: Path) -> None:
    """Render phone screenshots under the Android output root."""
    phone_dir = output_dir / "phone"
    phone_dir.mkdir(parents=True)
    subprocess.run(
        [
            sys.executable,
            "scripts/android_e2e.py",
            "--module",
            "gomode",
            "--screenshots",
            "--screenshot-dir",
            str(phone_dir),
            "--port",
            str(ANDROID_VISUAL_PORT),
        ],
        cwd=ROOT_DIR,
        check=True,
    )


def replace_baselines(source_dir: Path, baseline_dir: Path) -> None:
    """Replace the owned baseline image set with generated images."""
    generated = image_files(source_dir)
    existing = image_files(baseline_dir)
    for name in sorted(existing.keys() - generated.keys()):
        existing[name].unlink()
    for name, source in generated.items():
        destination = baseline_dir / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)


def preserve_failure(platform: str, first: Path, second: Path) -> Path:
    """Preserve failed render passes in the ignored failure directory."""
    destination = FAILURE_DIR / platform
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(first, destination / "first")
    shutil.copytree(second, destination / "second")
    return destination


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("check", "update", "generate"))
    parser.add_argument(
        "--platform",
        choices=("all", "android", "frontend"),
        default="all",
        help="visual platform to render; defaults to all",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.mode != "generate":
        # Only the comparison modes decode images, so this precheck belongs to
        # them. The renderers need ffmpeg independently (the documentation
        # screenshots are encoded to webp), so every environment that runs a
        # renderer, including the generate targets in CI, installs it.
        for executable in ("ffmpeg", "ffprobe"):
            if shutil.which(executable) is None:
                print(
                    f"{executable} is required for deterministic screenshot comparison",
                    file=sys.stderr,
                )
                return 1

    platforms = tuple(BASELINE_DIRS) if args.platform == "all" else (args.platform,)
    if "android" in platforms:
        try:
            verify_android_visual_environment()
        except (OSError, RuntimeError, subprocess.CalledProcessError) as exc:
            print(f"Android screenshot environment check failed: {exc}", file=sys.stderr)
            return 1
    renderers = {"android": render_android, "frontend": render_frontend}
    with tempfile.TemporaryDirectory(prefix="caic-visual-") as tmp:
        tmp_dir = Path(tmp)
        for platform in platforms:
            first = tmp_dir / platform / "first"
            second = tmp_dir / platform / "second"
            if args.mode == "generate":
                # A single pass: CI only needs the generators to run, and a
                # repeatability failure here would be an environment problem.
                print(f"Rendering {platform} screenshots...")
                renderers[platform](first)
                print(f"{platform} screenshots rendered.")
                continue
            if platform == "frontend":
                print("Rendering frontend screenshots (passes 1/2 and 2/2)...")
                render_frontend_passes(first, second)
            else:
                print(f"Rendering {platform} screenshots (pass 1/2)...")
                render_android(first)
                print(f"Rendering {platform} screenshots (pass 2/2)...")
                render_android(second)

            differences = compare_images(first, second, f"{platform} repeatability")
            if differences:
                print("\n".join(differences), file=sys.stderr)
                artifacts = preserve_failure(platform, first, second)
                print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
                return 1

            baseline_dir = BASELINE_DIRS[platform]
            if args.mode == "check":
                differences = compare_images(first, baseline_dir, f"{platform} baseline")
                if differences:
                    print("\n".join(differences), file=sys.stderr)
                    artifacts = preserve_failure(platform, first, second)
                    print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
                    print("Run 'make screenshots-update' to accept intentional changes.", file=sys.stderr)
                    return 1
                print(f"{platform} screenshots are deterministic and match their baselines.")
            else:
                replace_baselines(first, baseline_dir)
                print(f"Updated {platform} screenshot baselines.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
