"""Unit tests for the Android end-to-end launcher."""

import unittest

from android_e2e import ready_adb_serials

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


if __name__ == "__main__":
    unittest.main()
