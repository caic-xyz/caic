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
    ANDROID_SYSTEM_IMAGE_REVISION,
    ANDROID_WEBVIEW_VERSION,
    FRONTEND_SPECS,
    FRONTEND_VISUAL_HOST,
    MAX_LUMA_DELTA,
    MAX_LUMA_ERROR_PER_MILLION_PIXELS,
    android_system_image_path,
    current_webview_provider,
    image_files,
    luma_difference,
    parse_properties,
    render_android,
    render_frontend,
    render_frontend_passes,
    replace_baselines,
    require_available_loopback_ports,
    run_frontend_spec,
    verify_android_visual_environment,
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
                        platform="frontend",
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
                        platform="frontend",
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 1)
        self.assertIn("ffmpeg is required", stderr.getvalue())


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

    @mock.patch("visual_screenshots.subprocess.run")
    def test_render_android_routes_output_to_phone(self, run):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "android"

            render_android(output)

            command = run.call_args.args[0]
            screenshot_dir_index = command.index("--screenshot-dir") + 1
            self.assertEqual(command[screenshot_dir_index], str(output / "phone"))
            self.assertTrue((output / "phone").is_dir())


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


class AndroidVisualEnvironmentTest(unittest.TestCase):
    def setUp(self) -> None:
        # The fixtures below describe an x86_64 emulator, so pin the host architecture that selects
        # the expected ABI. Otherwise these tests fail on arm64 hosts such as macOS runners.
        machine = mock.patch("visual_screenshots.platform.machine", return_value="x86_64")
        machine.start()
        self.addCleanup(machine.stop)

    def test_parse_properties_ignores_comments_and_whitespace(self) -> None:
        self.assertEqual(
            parse_properties("# comment\n Pkg.Revision = 9 \nPkg.Path=system-images;android-35\n"),
            {"Pkg.Revision": "9", "Pkg.Path": "system-images;android-35"},
        )

    @mock.patch("visual_screenshots.platform.machine", return_value="aarch64")
    def test_system_image_path_selects_host_architecture(self, _machine: mock.Mock) -> None:

        path = android_system_image_path(Path("/sdk"))

        self.assertEqual(path, Path("/sdk/system-images/android-35/google_apis/arm64-v8a"))

    def test_current_webview_provider_parses_active_package(self) -> None:
        details = (
            "Current WebView Update Service state\n"
            "  Current WebView package (name, version): (com.google.android.webview, 124.0.6367.219)\n"
        )

        self.assertEqual(
            current_webview_provider(details),
            ("com.google.android.webview", "124.0.6367.219"),
        )

    @mock.patch("visual_screenshots.adb_output")
    @mock.patch("visual_screenshots.subprocess.run")
    @mock.patch("visual_screenshots.shutil.which", return_value="/sdk/platform-tools/adb")
    @mock.patch("visual_screenshots.android_avd_config_path")
    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_accepts_canonical_image_and_webview(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
        avd_config_path: mock.Mock,
        _which: mock.Mock,
        run: mock.Mock,
        adb_output_mock: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text(f"Pkg.Revision={ANDROID_SYSTEM_IMAGE_REVISION}\n")
            (image / "build.prop").write_text(
                "ro.system.build.fingerprint=google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys\n",
            )
            avd_config = Path(tmp) / "config.ini"
            avd_config.write_text("abi.type=x86_64\ntag.id=google_apis\ntarget=android-35\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image
            avd_config_path.return_value = avd_config
            run.return_value = mock.Mock(stdout="List of devices attached\nemulator-5554\tdevice\n")
            adb_output_mock.side_effect = [
                "caic_test\nOK",
                "35",
                "x86_64",
                "google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys",
                f"Current WebView package (name, version): (com.google.android.webview, {ANDROID_WEBVIEW_VERSION})",
            ]

            verify_android_visual_environment()

    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_rejects_wrong_system_image_revision(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text("Pkg.Revision=8\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image

            with self.assertRaisesRegex(RuntimeError, "expected 9, found 8"):
                verify_android_visual_environment()

    @mock.patch("visual_screenshots.adb_output")
    @mock.patch("visual_screenshots.subprocess.run")
    @mock.patch("visual_screenshots.shutil.which", return_value="/sdk/platform-tools/adb")
    @mock.patch("visual_screenshots.android_avd_config_path")
    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_rejects_wrong_webview_version(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
        avd_config_path: mock.Mock,
        _which: mock.Mock,
        run: mock.Mock,
        adb_output_mock: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text(f"Pkg.Revision={ANDROID_SYSTEM_IMAGE_REVISION}\n")
            (image / "build.prop").write_text(
                "ro.system.build.fingerprint=google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys\n",
            )
            avd_config = Path(tmp) / "config.ini"
            avd_config.write_text("abi.type=x86_64\ntag.id=google_apis\ntarget=android-35\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image
            avd_config_path.return_value = avd_config
            run.return_value = mock.Mock(stdout="List of devices attached\nemulator-5554\tdevice\n")
            adb_output_mock.side_effect = [
                "caic_test\nOK",
                "35",
                "x86_64",
                "google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys",
                "Current WebView package (name, version): (com.google.android.webview, 125.0.0.0)",
            ]

            with self.assertRaisesRegex(RuntimeError, f"expected com.google.android.webview {ANDROID_WEBVIEW_VERSION}"):
                verify_android_visual_environment()

    @mock.patch("visual_screenshots.adb_output")
    @mock.patch("visual_screenshots.subprocess.run")
    @mock.patch("visual_screenshots.shutil.which", return_value="/sdk/platform-tools/adb")
    @mock.patch("visual_screenshots.android_avd_config_path")
    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_rejects_alternate_active_webview_provider(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
        avd_config_path: mock.Mock,
        _which: mock.Mock,
        run: mock.Mock,
        adb_output_mock: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text(f"Pkg.Revision={ANDROID_SYSTEM_IMAGE_REVISION}\n")
            (image / "build.prop").write_text(
                "ro.system.build.fingerprint=google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys\n",
            )
            avd_config = Path(tmp) / "config.ini"
            avd_config.write_text("abi.type=x86_64\ntag.id=google_apis\ntarget=android-35\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image
            avd_config_path.return_value = avd_config
            run.return_value = mock.Mock(stdout="List of devices attached\nemulator-5554\tdevice\n")
            adb_output_mock.side_effect = [
                "caic_test\nOK",
                "35",
                "x86_64",
                "google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys",
                f"Current WebView package (name, version): (com.android.chrome, {ANDROID_WEBVIEW_VERSION})",
            ]

            with self.assertRaisesRegex(RuntimeError, "found com.android.chrome"):
                verify_android_visual_environment()

    @mock.patch("visual_screenshots.android_avd_config_path")
    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_rejects_same_name_avd_configured_with_wrong_image(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
        avd_config_path: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text(f"Pkg.Revision={ANDROID_SYSTEM_IMAGE_REVISION}\n")
            (image / "build.prop").write_text(
                "ro.system.build.fingerprint=google/sdk_gphone64_x86_64/emu64xa:15/build:userdebug/dev-keys\n",
            )
            avd_config = Path(tmp) / "config.ini"
            avd_config.write_text("abi.type=x86_64\ntag.id=default\ntarget=android-34\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image
            avd_config_path.return_value = avd_config

            with self.assertRaisesRegex(RuntimeError, "caic_test uses the wrong system image"):
                verify_android_visual_environment()

    @mock.patch("visual_screenshots.adb_output")
    @mock.patch("visual_screenshots.subprocess.run")
    @mock.patch("visual_screenshots.shutil.which", return_value="/sdk/platform-tools/adb")
    @mock.patch("visual_screenshots.android_avd_config_path")
    @mock.patch("visual_screenshots.android_sdk_root")
    @mock.patch("visual_screenshots.android_system_image_path")
    def test_rejects_runtime_from_different_system_image_revision(
        self,
        system_image_path: mock.Mock,
        sdk_root: mock.Mock,
        avd_config_path: mock.Mock,
        _which: mock.Mock,
        run: mock.Mock,
        adb_output_mock: mock.Mock,
    ) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / "image"
            image.mkdir()
            (image / "source.properties").write_text(f"Pkg.Revision={ANDROID_SYSTEM_IMAGE_REVISION}\n")
            (image / "build.prop").write_text(
                "ro.system.build.fingerprint=google/sdk_gphone64_x86_64/emu64xa:15/revision9:userdebug/dev-keys\n",
            )
            avd_config = Path(tmp) / "config.ini"
            avd_config.write_text("abi.type=x86_64\ntag.id=google_apis\ntarget=android-35\n")
            sdk_root.return_value = Path(tmp)
            system_image_path.return_value = image
            avd_config_path.return_value = avd_config
            run.return_value = mock.Mock(stdout="List of devices attached\nemulator-5554\tdevice\n")
            adb_output_mock.side_effect = [
                "caic_test\nOK",
                "35",
                "x86_64",
                "google/sdk_gphone64_x86_64/emu64xa:15/revision8:userdebug/dev-keys",
            ]

            with self.assertRaisesRegex(RuntimeError, "expected installed image .*revision9"):
                verify_android_visual_environment()


if __name__ == "__main__":
    unittest.main()
