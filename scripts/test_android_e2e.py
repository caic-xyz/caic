"""Unit tests for Android E2E device discovery and fake fixture selection."""

import os
import unittest
from unittest import mock

from android_e2e import backend_environment, ready_adb_serials

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


if __name__ == "__main__":
    unittest.main()
