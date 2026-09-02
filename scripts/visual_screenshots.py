#!/usr/bin/env python3
"""Check or update deterministic frontend and Android documentation screenshots."""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
ANDROID_VISUAL_PORT = 41743
BASELINE_DIRS = {
    "android": ROOT_DIR / "e2e" / "screenshots" / "android",
    "frontend": ROOT_DIR / "e2e" / "screenshots" / "frontend",
}
IMAGE_SUFFIXES = frozenset({".avif", ".png", ".webp"})
VISUAL_SEED = "caic-visual-v1"
FAILURE_DIR = ROOT_DIR / "test-results" / "visual-screenshots"
MAX_LUMA_DELTA = 2
MAX_LUMA_ERROR_PER_MILLION_PIXELS = 20


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


def render_frontend(output_dir: Path) -> None:
    """Render the frontend visual tests into output_dir."""
    output_dir.mkdir(parents=True)
    env = os.environ.copy()
    env.update(
        {
            "CAIC_E2E_SEED": VISUAL_SEED,
            "CAIC_E2E_VISUALS": "1",
            "CAIC_SCREENSHOT_DIR": str(output_dir),
            "CI": "",
            "LC_ALL": "C.UTF-8",
            "TZ": "UTC",
        },
    )
    for spec in ("e2e/tests/gen-screenshots.spec.ts", "e2e/tests/prompt-input.spec.ts"):
        subprocess.run(
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
            cwd=ROOT_DIR,
            env=env,
            check=True,
        )


def render_android(output_dir: Path) -> None:
    """Render the Android documentation screenshot test into output_dir."""
    output_dir.mkdir(parents=True)
    subprocess.run(
        [
            sys.executable,
            "scripts/android_e2e.py",
            "--module",
            "gomode",
            "--screenshots",
            "--screenshot-dir",
            str(output_dir),
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
    """Preserve failed render passes in the ignored test-results directory."""
    destination = FAILURE_DIR / platform
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(first, destination / "first")
    shutil.copytree(second, destination / "second")
    return destination


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("check", "update"))
    parser.add_argument(
        "--platform",
        choices=("all", "android", "frontend"),
        default="all",
        help="visual platform to render; defaults to all",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    for executable in ("ffmpeg", "ffprobe"):
        if shutil.which(executable) is None:
            print(
                f"{executable} is required for deterministic screenshot comparison",
                file=sys.stderr,
            )
            return 1

    platforms = tuple(BASELINE_DIRS) if args.platform == "all" else (args.platform,)
    renderers = {"android": render_android, "frontend": render_frontend}
    with tempfile.TemporaryDirectory(prefix="caic-visual-") as tmp:
        tmp_dir = Path(tmp)
        for platform in platforms:
            first = tmp_dir / platform / "first"
            second = tmp_dir / platform / "second"
            print(f"Rendering {platform} screenshots (pass 1/2)...")
            renderers[platform](first)
            print(f"Rendering {platform} screenshots (pass 2/2)...")
            renderers[platform](second)

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
                    print("Run 'make screenshots-update' to accept intentional changes.", file=sys.stderr)
                    return 1
                print(f"{platform} screenshots are deterministic and match their baselines.")
            else:
                replace_baselines(first, baseline_dir)
                print(f"Updated {platform} screenshot baselines.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
