"""Unit tests for deterministic screenshot luminance comparison and render modes."""

import argparse
import socket
import subprocess
import tempfile
import threading
import unittest
from contextlib import redirect_stderr, redirect_stdout
from io import StringIO
from pathlib import Path
from unittest import mock

import visual_screenshots
from visual_screenshots import (
    FRONTEND_SPECS,
    FRONTEND_VISUAL_HOST,
    MAX_LUMA_DELTA,
    MAX_LUMA_ERROR_PER_MILLION_PIXELS,
    image_files,
    luma_difference,
    render_frontend,
    render_frontend_passes,
    replace_baselines,
    require_available_loopback_ports,
    run_frontend_spec,
)


class LumaDifferenceTest(unittest.TestCase):
    def test_identical_pixels_are_accepted(self):
        difference = luma_difference(bytes((0, 128, 255)), bytes((0, 128, 255)))

        self.assertTrue(difference.acceptable)
        self.assertEqual(difference.maximum, 0)
        self.assertEqual(difference.total, 0)

    def test_observed_repeatability_bound_is_accepted(self):
        actual = bytes(1_000_000)
        expected = bytearray(actual)
        expected[: MAX_LUMA_ERROR_PER_MILLION_PIXELS // MAX_LUMA_DELTA] = bytes(
            [MAX_LUMA_DELTA] * (MAX_LUMA_ERROR_PER_MILLION_PIXELS // MAX_LUMA_DELTA),
        )

        difference = luma_difference(actual, expected)

        self.assertTrue(difference.acceptable)
        self.assertEqual(difference.total, MAX_LUMA_ERROR_PER_MILLION_PIXELS)

    def test_excessive_single_pixel_delta_is_rejected(self):
        difference = luma_difference(bytes((0,)), bytes((MAX_LUMA_DELTA + 1,)))

        self.assertFalse(difference.acceptable)

    def test_excessive_total_delta_is_rejected(self):
        actual = bytes(1_000_000)
        expected = bytearray(actual)
        expected[: MAX_LUMA_ERROR_PER_MILLION_PIXELS + 1] = bytes(
            [1] * (MAX_LUMA_ERROR_PER_MILLION_PIXELS + 1),
        )

        difference = luma_difference(actual, expected)

        self.assertFalse(difference.acceptable)

    def test_different_lengths_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "different lengths"):
            luma_difference(bytes((0,)), bytes((0, 0)))


class GenerateModeTest(unittest.TestCase):
    """The generate mode renders without the comparison precheck.

    Only the comparison modes decode images, so they require ffprobe/ffmpeg up
    front; the generate mode leaves that to each renderer, which needs ffmpeg on
    its own to encode the screenshots. The renderers are stubbed here, so these
    tests pin the mode split and not the renderer tooling.
    """

    def test_generate_renders_once_without_the_comparison_precheck(self):
        rendered: list[str] = []
        stdout = StringIO()

        def render(directory):
            directory.mkdir(parents=True)
            rendered.append(str(directory))

        with (
            redirect_stdout(stdout),
            mock.patch.object(visual_screenshots.shutil, "which", return_value=None),
            mock.patch.dict(
                visual_screenshots.__dict__,
                {"render_frontend": render},
            ),
            mock.patch.dict(
                visual_screenshots.__dict__,
                {
                    "parse_args": lambda: argparse.Namespace(
                        mode="generate",
                        rebuilt=True,
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 0)
        self.assertEqual(len(rendered), 1)
        self.assertIn("frontend screenshots rendered.", stdout.getvalue())

    def test_check_still_requires_ffmpeg(self):
        stderr = StringIO()

        with (
            redirect_stderr(stderr),
            mock.patch.object(visual_screenshots.shutil, "which", return_value=None),
            mock.patch.dict(
                visual_screenshots.__dict__,
                {
                    "parse_args": lambda: argparse.Namespace(
                        mode="check",
                        rebuilt=True,
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 1)
        self.assertIn("ffmpeg is required", stderr.getvalue())


class FrontendBundleFreshnessTest(unittest.TestCase):
    """A comparison is only meaningful against a bundle built from the tree.

    The renderer serves the built bundle, so comparing while the sources are
    ahead of it would report on code nobody is looking at.
    """

    def test_unchanged_sources_are_accepted(self):
        self.assertIsNone(visual_screenshots.frontend_bundle_staleness([], []))

    def test_a_rebuilt_bundle_is_accepted(self):
        self.assertIsNone(
            visual_screenshots.frontend_bundle_staleness(
                ["frontend/src/App.tsx"],
                ["backend/frontend/dist/assets/index.js.br"],
            ),
        )

    def test_dirty_sources_with_a_clean_bundle_are_reported(self):
        problem = visual_screenshots.frontend_bundle_staleness(["frontend/src/App.tsx"], [])
        self.assertIsNotNone(problem)
        self.assertIn("frontend/src/App.tsx", problem)

    def test_main_refuses_a_stale_bundle_before_rendering(self):
        stderr = StringIO()

        def changed(paths):
            return ["frontend/src/App.tsx"] if paths == visual_screenshots.FRONTEND_BUILD_INPUTS else []

        with (
            redirect_stderr(stderr),
            mock.patch.object(visual_screenshots, "changed_paths", side_effect=changed),
            mock.patch.object(visual_screenshots.shutil, "which", return_value="/usr/bin/ffmpeg"),
            mock.patch.dict(
                visual_screenshots.__dict__,
                {
                    "parse_args": lambda: argparse.Namespace(
                        mode="check",
                        platform="frontend",
                        rebuilt=False,
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 1)
        self.assertIn("Refusing to render frontend screenshots", stderr.getvalue())


class ScreenshotTreeTest(unittest.TestCase):
    def test_image_files_keeps_recursive_relative_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "desktop").mkdir()
            (root / "mobile").mkdir()
            (root / "desktop" / "detail.webp").write_bytes(b"desktop")
            (root / "mobile" / "detail.png").write_bytes(b"mobile")
            (root / "mobile" / "notes.txt").write_text("ignored")

            self.assertEqual(
                list(image_files(root)),
                ["desktop/detail.webp", "mobile/detail.png"],
            )

    def test_replace_baselines_replaces_recursive_owned_images(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            baseline = root / "baseline"
            (source / "desktop").mkdir(parents=True)
            (source / "mobile").mkdir()
            (baseline / "desktop").mkdir(parents=True)
            (baseline / "legacy.webp").write_bytes(b"stale")
            (baseline / "desktop" / "detail.webp").write_bytes(b"old")
            (source / "desktop" / "detail.webp").write_bytes(b"new")
            (source / "mobile" / "detail.webp").write_bytes(b"mobile")

            replace_baselines(source, baseline)

            self.assertEqual(
                {name: path.read_bytes() for name, path in image_files(baseline).items()},
                {
                    "desktop/detail.webp": b"new",
                    "mobile/detail.webp": b"mobile",
                },
            )


class FrontendRenderingTest(unittest.TestCase):
    def test_reserved_loopback_port_collision_is_actionable(self) -> None:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
            listener.bind((FRONTEND_VISUAL_HOST, 0))
            port = listener.getsockname()[1]

            with self.assertRaisesRegex(RuntimeError, f"port {port} is unavailable"):
                require_available_loopback_ports((port,))

    def test_reserved_loopback_port_allows_immediate_retry(self) -> None:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind((FRONTEND_VISUAL_HOST, 0))
            listener.listen()
            port = listener.getsockname()[1]
            with socket.create_connection((FRONTEND_VISUAL_HOST, port)) as client:
                connection, _ = listener.accept()
                connection.close()
                client.shutdown(socket.SHUT_WR)

        require_available_loopback_ports((port,))

    @mock.patch("visual_screenshots.run_frontend_spec")
    def test_render_frontend_uses_isolated_port_and_artifacts(self, run: mock.Mock) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "frontend" / "first"

            render_frontend(output, 32123)

        self.assertEqual(run.call_count, len(FRONTEND_SPECS))
        for call, spec in zip(run.call_args_list, FRONTEND_SPECS, strict=True):
            command, env = call.args
            self.assertIn(spec, command)
            self.assertEqual(env["CAIC_E2E_HOST"], FRONTEND_VISUAL_HOST)
            self.assertEqual(env["CAIC_E2E_PORT"], "32123")
            self.assertEqual(env["CAIC_SCREENSHOT_DIR"], str(output))
            self.assertTrue(env["CAIC_E2E_OUTPUT_DIR"].endswith("playwright/frontend/first"))

    @mock.patch("visual_screenshots.subprocess.run")
    def test_frontend_spec_success_is_quiet(self, run: mock.Mock) -> None:
        run.return_value = mock.Mock(returncode=0, stdout="routine output\n", stderr="routine error\n")
        stderr = StringIO()

        with redirect_stderr(stderr):
            run_frontend_spec(["playwright", "test"], {"PATH": "/bin"})

        self.assertEqual(stderr.getvalue(), "")

    @mock.patch("visual_screenshots.subprocess.run")
    def test_frontend_spec_failure_replays_full_output(self, run: mock.Mock) -> None:
        result = mock.Mock(returncode=1, stdout="server diagnostics\n", stderr="test diagnostics\n")
        result.check_returncode.side_effect = subprocess.CalledProcessError(1, ["playwright", "test"])
        run.return_value = result
        stderr = StringIO()

        with self.assertRaises(subprocess.CalledProcessError), redirect_stderr(stderr):
            run_frontend_spec(["playwright", "test"], {"PATH": "/bin"})

        self.assertEqual(stderr.getvalue(), "server diagnostics\ntest diagnostics\n")

    @mock.patch("visual_screenshots.FRONTEND_VISUAL_PORTS", (31001, 31002))
    @mock.patch("visual_screenshots.require_available_loopback_ports")
    @mock.patch("visual_screenshots.render_frontend")
    def test_frontend_passes_run_concurrently(self, render: mock.Mock, available: mock.Mock) -> None:
        barrier = threading.Barrier(2, timeout=1)
        render.side_effect = lambda _output, _port: barrier.wait()

        render_frontend_passes(Path("first"), Path("second"))

        self.assertCountEqual(
            render.call_args_list,
            [mock.call(Path("first"), 31001), mock.call(Path("second"), 31002)],
        )
        available.assert_called_once_with((31001, 31002))
