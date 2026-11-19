#!/usr/bin/env python3
"""Render and publish deterministic light and dark frontend documentation screenshots.

Renders twice, requires both passes to match, and publishes the first. The
tracked images encode the development container's font stack, so publish them
from that container."""

import argparse
import concurrent.futures
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from screenshot_publish import publish_images, recover_images, validate_images

ROOT_DIR = Path(__file__).resolve().parent.parent
FRONTEND_VISUAL_PORTS = (41741, 41742)
FRONTEND_VISUAL_HOST = "127.0.0.1"
SCREENSHOT_DIR = ROOT_DIR / "e2e" / "screenshots" / "frontend"
IMAGE_SUFFIXES = frozenset({".avif", ".png", ".webp"})
# Each scene must publish a matched light and dark image. The widget animation is a single recording.
FRONTEND_SCENES = frozenset(
    {
        "desktop/overview",
        "desktop/prompt-detail-long",
        "desktop/prompt-detail-short",
        "desktop/prompt-long",
        "desktop/prompt-short",
        "desktop/settings-general",
        "desktop/settings-mounts",
        "desktop/settings-server-error",
        "desktop/settings-server",
        "desktop/task-ask",
        "desktop/task-background",
        "desktop/task-detail",
        "desktop/task-list-scrolled",
        "desktop/task-native-subagents",
        "desktop/task-repository-changes",
        "desktop/task-resources",
        "desktop/task-running",
        "desktop/task-stats",
        "desktop/task-vnc",
        "desktop/task-widget",
        "desktop/usage",
        "mobile/overview",
        "mobile/settings-general",
        "mobile/settings-mounts-mobile",
        "mobile/settings-server-error-mobile",
        "mobile/settings-server-mobile",
        "mobile/task-background",
        "mobile/task-detail-header-compact",
        "mobile/task-detail",
        "mobile/task-list-scrolled-mobile",
        "mobile/task-repository-changes",
        "mobile/task-resources",
        "mobile/task-stats",
        "mobile/usage",
    },
)
FRONTEND_SCENE_FILES = frozenset(
    f"{scene}-{scheme}.webp" for scene in FRONTEND_SCENES for scheme in ("light", "dark")
) | {"desktop/task-widget.avif"}
VISUAL_SEED = "caic-visual-v1"
# Playwright deletes test-results/ at the start of every run, so failed renders
# live beside it instead of inside it: they have to survive a re-run to be
# inspected.
FAILURE_DIR = ROOT_DIR / "visual-screenshots-failures"
MAX_LUMA_DELTA = 2
MAX_LUMA_ERROR_PER_MILLION_PIXELS = 20
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
    if actual == expected:
        return LumaDifference(maximum=0, pixel_count=len(actual), total=0)
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
        if actual[name].read_bytes() == expected[name].read_bytes():
            continue
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


def render_frontend(output_dir: Path, port: int, binary: Path) -> None:
    """Render the frontend visual tests into output_dir."""
    output_dir.mkdir(parents=True)
    env = os.environ.copy()
    env.update(
        {
            "CAIC_E2E_BINARY": str(binary),
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
    run_frontend_spec(
        ["pnpm", "exec", "playwright", "test", "--config", "e2e/playwright.config.ts", *FRONTEND_SPECS, "--workers=1"],
        env,
    )


def render_frontend_passes(first: Path, second: Path, binary: Path) -> None:
    """Render both isolated frontend passes concurrently."""
    require_available_loopback_ports(FRONTEND_VISUAL_PORTS)
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        futures = [
            executor.submit(render_frontend, output, port, binary)
            for output, port in zip((first, second), FRONTEND_VISUAL_PORTS, strict=True)
        ]
        for future in concurrent.futures.as_completed(futures):
            future.result()


def image_dimensions(path: Path) -> tuple[int, int]:
    """Return dimensions of the first image/video stream."""
    width, height, _ = video_layout(path).strip().splitlines()[0].split(",")
    return int(width), int(height)


def validate_frontend_images(directory: Path) -> None:
    """Validate the inventory and check each new pair shows a distinct, equally sized image."""
    dimensions: dict[str, tuple[int, int]] = {}

    def measure(path: Path) -> tuple[int, int]:
        size = image_dimensions(path)
        dimensions[path.relative_to(directory).as_posix()] = size
        return size

    validate_images(directory, FRONTEND_SCENE_FILES, IMAGE_SUFFIXES, measure)
    for scene in sorted(FRONTEND_SCENES):
        light = f"{scene}-light.webp"
        dark = f"{scene}-dark.webp"
        if dimensions[light] != dimensions[dark]:
            raise RuntimeError(f"Screenshot pair has different dimensions: {scene}")
        if (directory / light).read_bytes() == (directory / dark).read_bytes():
            raise RuntimeError(f"Screenshot pair has identical images: {scene}")


def validate_previous_images(directory: Path) -> None:
    """Allow recovery of an earlier catalog with different scene names."""
    validate_images(directory, None, IMAGE_SUFFIXES, image_dimensions)


def publish_screenshots(source_dir: Path) -> None:
    """Publish validated frontend images while retaining recoverable prior images."""
    publish_images(
        source_dir,
        ROOT_DIR,
        SCREENSHOT_DIR,
        validate_frontend_images,
        validate_previous_images,
    )


def preserve_failure(platform: str, first: Path, second: Path) -> Path:
    """Preserve failed render passes in the ignored failure directory."""
    destination = FAILURE_DIR / platform
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(first, destination / "first")
    shutil.copytree(second, destination / "second")
    return destination


def main() -> int:
    argparse.ArgumentParser(description=__doc__).parse_args()
    started = time.monotonic()
    # The renderers encode webp and the repeatability comparison decodes it.
    for executable in ("ffmpeg", "ffprobe"):
        if shutil.which(executable) is None:
            print(f"{executable} is required for documentation screenshots", file=sys.stderr)
            return 1
    recover_images(ROOT_DIR, SCREENSHOT_DIR, validate_previous_images)

    with tempfile.TemporaryDirectory(prefix="caic-visual-") as tmp:
        tmp_dir = Path(tmp)
        binary = tmp_dir / "caic-fake"
        subprocess.run(
            ["go", "build", "-tags", "e2e", "-o", str(binary), "./backend/cmd/caic"], cwd=ROOT_DIR, check=True
        )
        first = tmp_dir / "frontend" / "first"
        second = tmp_dir / "frontend" / "second"
        print("Rendering frontend screenshots (passes 1/2 and 2/2)...")
        render_frontend_passes(first, second, binary)

        differences = compare_images(first, second, "frontend repeatability")
        if differences:
            print("\n".join(differences), file=sys.stderr)
            artifacts = preserve_failure("frontend", first, second)
            print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
            return 1

        publish_screenshots(first)
        print("Updated frontend screenshots.")
    print(f"Frontend screenshots update: {time.monotonic() - started:.2f}s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
