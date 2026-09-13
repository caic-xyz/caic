"""Unit tests for Android E2E device discovery and fake fixture selection."""

import os
import unittest
from unittest import mock

from android_e2e import allocate_reverse_port, backend_environment, ready_adb_serials

ADB_DEVICES = """List of devices attached
emulator-5554\tdevice product:sdk model:sdk transport_id:7
emulator-5556\tdevice product:sdk model:sdk transport_id:8
emulator-5558\toffline transport_id:9
"""


class ReadyAdbSerialsTest(unittest.TestCase):
    def test_returns_every_ready_device_without_selection(self):
        self.assertEqual(
            ready_adb_serials(ADB_DEVICES, None),
            ["emulator-5554", "emulator-5556"],
        )

    def test_honors_android_serial_selection(self):
        self.assertEqual(
            ready_adb_serials(ADB_DEVICES, "emulator-5556"),
            ["emulator-5556"],
        )

    def test_rejects_missing_or_unready_selection(self):
        self.assertEqual(ready_adb_serials(ADB_DEVICES, "emulator-5558"), [])
        self.assertEqual(ready_adb_serials(ADB_DEVICES, "emulator-5560"), [])


class BackendEnvironmentTest(unittest.TestCase):
    def test_enables_legacy_fixtures_for_screenshots(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(backend_environment(True)["CAIC_E2E_VISUALS"], "1")

    def test_disables_legacy_fixtures_for_behavioral_tests(self):
        with mock.patch.dict(os.environ, {"CAIC_E2E_VISUALS": "1"}, clear=True):
            self.assertNotIn("CAIC_E2E_VISUALS", backend_environment(False))


class AllocateReversePortTest(unittest.TestCase):
    @mock.patch("android_e2e.subprocess.run")
    def test_allocates_temporary_device_port_for_temporary_backend(self, run: mock.Mock):
        run.return_value = mock.Mock(stdout="45543\n")

        port = allocate_reverse_port("emulator-5554", 48413, None)

        self.assertEqual(port, 45543)
        run.assert_called_once_with(
            ["adb", "-s", "emulator-5554", "reverse", "tcp:0", "tcp:48413"],
            capture_output=True,
            check=True,
            text=True,
        )

    @mock.patch("android_e2e.subprocess.run")
    def test_uses_explicit_device_port_for_stable_visual_urls(self, run: mock.Mock):
        run.return_value = mock.Mock(stdout="")

        port = allocate_reverse_port("device.local:5555", 41743, 41743)

        self.assertEqual(port, 41743)
        run.assert_called_once_with(
            ["adb", "-s", "device.local:5555", "reverse", "tcp:41743", "tcp:41743"],
            capture_output=True,
            check=True,
            text=True,
        )

    @mock.patch("android_e2e.subprocess.run")
    def test_rejects_invalid_allocated_port(self, run: mock.Mock):
        run.return_value = mock.Mock(stdout="not-a-port\n")

        with self.assertRaisesRegex(RuntimeError, "unexpected port"):
            allocate_reverse_port("emulator-5554", 48413, None)


if __name__ == "__main__":
    unittest.main()
