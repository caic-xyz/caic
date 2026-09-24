#!/usr/bin/env python3
"""Render, check, or update deterministic frontend documentation screenshots.

Baseline comparison is a maintainer check: the tracked images encode the
development container's font stack, so comparing renders from a different host
reports typeface differences that are not product regressions. The generate mode
renders without comparing, which is what CI gates so a stale generator cannot
rot silently."""

import argparse
import concurrent.futures
import os
import shutil
import socket
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
FRONTEND_VISUAL_PORTS = (41741, 41742)
FRONTEND_VISUAL_HOST = "127.0.0.1"
# The frontend platform renders whatever bundle `pnpm build` last wrote, so a
# comparison says nothing about the working tree once these inputs move ahead of
# it. The preflight in main() refuses that case instead of rendering the
# committed bundle and reporting a misleading result. Test files are excluded
# because they are not bundled.
FRONTEND_BUILD_INPUTS = (
    "frontend/src",
    "frontend/index.html",
    "vite.config.ts",
    "package.json",
    "sdk/caic/ts",
    "sdk/mcp/ts",
    "sdk/voicegateway/ts",
    ":(exclude,glob)frontend/src/**/*.test.ts",
    ":(exclude,glob)frontend/src/**/*.test.tsx",
)
FRONTEND_BUNDLE = "backend/frontend/dist"
BASELINE_DIR = ROOT_DIR / "e2e" / "screenshots" / "frontend"
IMAGE_SUFFIXES = frozenset({".avif", ".png", ".webp"})
VISUAL_SEED = "caic-visual-v1"
# Playwright deletes test-results/ at the start of every run, so failed renders
# live beside it instead of inside it: they have to survive a re-run to be
# inspected, and CI uploads this directory when a check fails.
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


def changed_paths(paths: tuple[str, ...]) -> list[str]:
    """Return the uncommitted paths under paths, keeping pathspec magic intact."""
    result = subprocess.run(
        ["git", "-C", str(ROOT_DIR), "status", "--porcelain", "--", *paths],
        capture_output=True,
        check=True,
        text=True,
    )
    return [line[3:] for line in result.stdout.splitlines() if line.strip()]


def frontend_bundle_staleness(source_changes: list[str], bundle_changes: list[str]) -> str | None:
    """Return why the built bundle cannot contain the tree, or None when it can."""
    if not source_changes or bundle_changes:
        return None
    return (
        f"{len(source_changes)} frontend input(s) changed while the built bundle stayed untouched, "
        f"starting with {source_changes[0]}"
    )


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
        "--rebuilt",
        action="store_true",
        help="the caller rebuilt the frontend bundle immediately before rendering",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if not args.rebuilt:
        problem = frontend_bundle_staleness(
            changed_paths(FRONTEND_BUILD_INPUTS),
            changed_paths((FRONTEND_BUNDLE,)),
        )
        if problem:
            print(f"Refusing to render frontend screenshots: {problem}.", file=sys.stderr)
            print("Run a screenshots make target, which builds the bundle first.", file=sys.stderr)
            return 1
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

    with tempfile.TemporaryDirectory(prefix="caic-visual-") as tmp:
        tmp_dir = Path(tmp)
        first = tmp_dir / "frontend" / "first"
        second = tmp_dir / "frontend" / "second"
        if args.mode == "generate":
            print("Rendering frontend screenshots...")
            render_frontend(first)
            print("frontend screenshots rendered.")
            return 0
        print("Rendering frontend screenshots (passes 1/2 and 2/2)...")
        render_frontend_passes(first, second)

        differences = compare_images(first, second, "frontend repeatability")
        if differences:
            print("\n".join(differences), file=sys.stderr)
            artifacts = preserve_failure("frontend", first, second)
            print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
            return 1

        if args.mode == "check":
            differences = compare_images(first, BASELINE_DIR, "frontend baseline")
            if differences:
                print("\n".join(differences), file=sys.stderr)
                artifacts = preserve_failure("frontend", first, second)
                print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
                print("Run 'make screenshots-update' to accept intentional changes.", file=sys.stderr)
                return 1
            print("frontend screenshots are deterministic and match their baselines.")
        else:
            replace_baselines(first, BASELINE_DIR)
            print("Updated frontend screenshot baselines.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
