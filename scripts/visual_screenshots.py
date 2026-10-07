#!/usr/bin/env python3
"""Render, check, or update deterministic frontend documentation screenshots.

Baseline comparison is a maintainer check: the tracked images encode the
development container's font stack, so comparing renders from a different host
reports typeface differences that are not product regressions. The generate mode
renders without comparing, which is what CI gates so a stale generator cannot
rot silently."""

import argparse
import concurrent.futures
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from functools import partial
from pathlib import Path

from screenshot_catalog import CatalogSpec, publish_catalog, recover_catalog, validate_catalog

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
# Every declared documentation scene is required, including prompt and animation captures.
FRONTEND_SCENE_FILES = frozenset(
    {
        "desktop/overview.webp",
        "desktop/prompt-detail-long.webp",
        "desktop/prompt-detail-short.webp",
        "desktop/prompt-long.webp",
        "desktop/prompt-short.webp",
        "desktop/settings-general.webp",
        "desktop/settings-mounts.webp",
        "desktop/settings-server-error.webp",
        "desktop/settings-server.webp",
        "desktop/task-ask.webp",
        "desktop/task-detail.webp",
        "desktop/task-list-scrolled.webp",
        "desktop/task-native-subagents.webp",
        "desktop/task-repository-changes.webp",
        "desktop/task-resources.webp",
        "desktop/task-running.webp",
        "desktop/task-stats.webp",
        "desktop/task-vnc.webp",
        "desktop/task-widget.avif",
        "desktop/task-widget.webp",
        "desktop/usage.webp",
        "mobile/overview.webp",
        "mobile/settings-general.webp",
        "mobile/settings-mounts-mobile.webp",
        "mobile/settings-server-error-mobile.webp",
        "mobile/settings-server-mobile.webp",
        "mobile/task-detail-header-compact.webp",
        "mobile/task-detail.webp",
        "mobile/task-list-scrolled-mobile.webp",
        "mobile/task-repository-changes.webp",
        "mobile/task-resources.webp",
        "mobile/task-stats.webp",
        "mobile/usage.webp",
    },
)
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


def render_frontend(output_dir: Path, port: int = FRONTEND_VISUAL_PORTS[0], binary: Path | None = None) -> None:
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
    if binary is not None:
        env["CAIC_E2E_BINARY"] = str(binary)
    run_frontend_spec(
        ["pnpm", "exec", "playwright", "test", "--config", "e2e/playwright.config.ts", *FRONTEND_SPECS, "--workers=1"],
        env,
    )


def render_frontend_passes(first: Path, second: Path, binary: Path | None = None) -> None:
    """Render both isolated frontend passes concurrently."""
    require_available_loopback_ports(FRONTEND_VISUAL_PORTS)
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        futures = [
            executor.submit(render_frontend, output, port, binary)
            for output, port in zip((first, second), FRONTEND_VISUAL_PORTS, strict=True)
        ]
        for future in concurrent.futures.as_completed(futures):
            future.result()


def catalog_dimensions(path: Path) -> tuple[int, int]:
    """Return dimensions of the first image/video stream in a scene."""
    width, height, _ = video_layout(path).strip().splitlines()[0].split(",")
    return int(width), int(height)


def validate_frontend_catalog(directory: Path, expected_files: frozenset[str] | None) -> dict:
    """Validate the complete frontend catalog against its owned images."""
    spec = CatalogSpec("caic", "web", IMAGE_SUFFIXES, expected_files)
    catalog = validate_catalog(directory, spec, catalog_dimensions)
    for scene in catalog["scenarios"]:
        path = Path(scene["file"])
        name = path.with_suffix("").as_posix() + ("-animation" if path.suffix == ".avif" else "")
        if scene["name"] != name:
            raise RuntimeError("Frontend screenshot scene name disagrees with its file")
    return catalog


def replace_baselines(source_dir: Path, baseline_dir: Path) -> None:
    """Publish a validated frontend catalog while retaining recoverable prior images."""
    if baseline_dir != BASELINE_DIR:
        raise RuntimeError("Only the managed frontend baseline directory may be published")
    publish_catalog(
        source_dir,
        ROOT_DIR,
        baseline_dir,
        partial(validate_frontend_catalog, expected_files=FRONTEND_SCENE_FILES),
        partial(validate_frontend_catalog, expected_files=None),
    )


def preserve_failure(platform: str, first: Path, second: Path) -> Path:
    """Preserve failed render passes in the ignored failure directory."""
    destination = FAILURE_DIR / platform
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(first, destination / "first")
    shutil.copytree(second, destination / "second")
    return destination


def write_manifest(directory: Path) -> None:
    """Publish image dimensions, content hashes, and capture-input provenance."""
    files = subprocess.run(
        [
            "git",
            "ls-files",
            "-co",
            "--exclude-standard",
            "--",
            "backend",
            "e2e",
            "frontend",
            "scripts/run-dev.py",
            "scripts/visual_screenshots.py",
            "scripts/screenshot_catalog.py",
            "pnpm-lock.yaml",
            *FRONTEND_BUILD_INPUTS,
        ],
        cwd=ROOT_DIR,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.splitlines()
    digest = hashlib.sha256()
    for name in sorted(set(files)):
        source = ROOT_DIR / name
        if source.is_file() and not name.startswith("e2e/screenshots/"):
            digest.update(name.encode() + b"\0" + source.read_bytes() + b"\0")
    revision = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=ROOT_DIR, check=True, capture_output=True, text=True
    ).stdout.strip()
    scenarios = []
    for name, source in image_files(directory).items():
        width, height, _ = video_layout(source).strip().splitlines()[0].split(",")
        scenarios.append(
            {
                "name": str(Path(name).with_suffix("")) + ("-animation" if source.suffix == ".avif" else ""),
                "file": name,
                "width": int(width),
                "height": int(height),
                "sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
                "description": Path(name).stem.replace("-", " "),
            }
        )
    manifest = {
        "schemaVersion": 1,
        "producer": "caic",
        "platform": "web",
        "source": {"revision": revision, "inputsSha256": digest.hexdigest()},
        "scenarios": scenarios,
    }
    (directory / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


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
    started = time.monotonic()
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

    committed = None
    if args.mode == "check":
        committed = validate_frontend_catalog(BASELINE_DIR, FRONTEND_SCENE_FILES)
    elif args.mode == "update":
        recover_catalog(ROOT_DIR, BASELINE_DIR, partial(validate_frontend_catalog, expected_files=None))

    with tempfile.TemporaryDirectory(prefix="caic-visual-") as tmp:
        tmp_dir = Path(tmp)
        binary = tmp_dir / "caic-fake"
        subprocess.run(
            ["go", "build", "-tags", "e2e", "-o", str(binary), "./backend/cmd/caic"], cwd=ROOT_DIR, check=True
        )
        first = tmp_dir / "frontend" / "first"
        second = tmp_dir / "frontend" / "second"
        if args.mode == "generate":
            print("Rendering frontend screenshots...")
            render_frontend(first, binary=binary)
            write_manifest(first)
            print("frontend screenshots rendered.")
            return 0
        print("Rendering frontend screenshots (passes 1/2 and 2/2)...")
        render_frontend_passes(first, second, binary)
        write_manifest(first)

        differences = compare_images(first, second, "frontend repeatability")
        if differences:
            print("\n".join(differences), file=sys.stderr)
            artifacts = preserve_failure("frontend", first, second)
            print(f"Failed render artifacts: {artifacts}", file=sys.stderr)
            return 1

        if args.mode == "check":
            generated = json.loads((first / "manifest.json").read_text(encoding="utf-8"))
            if committed is None or committed["source"]["inputsSha256"] != generated["source"]["inputsSha256"]:
                raise RuntimeError("Frontend capture inputs changed; run make screenshots-update")
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
    print(f"Frontend screenshots {args.mode}: {time.monotonic() - started:.2f}s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
