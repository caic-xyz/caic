#!/usr/bin/env python3
"""Unit tests for Android emulator discovery used by the E2E launcher."""

import subprocess
import unittest
from unittest.mock import patch

import android_start_emulator


class RunningAVDSerialTest(unittest.TestCase):
    @patch("android_start_emulator.subprocess.run")
    def test_returns_matching_running_avd_serial(self, run: unittest.mock.Mock) -> None:
        run.side_effect = [
            subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout="List of devices attached\nemulator-5554\tdevice\n",
            ),
            subprocess.CompletedProcess(args=[], returncode=0, stdout="caic_test\nOK\n"),
        ]

        serial = android_start_emulator._running_avd_serial("adb")

        self.assertEqual("emulator-5554", serial)

    @patch("android_start_emulator.subprocess.run")
    def test_ignores_other_running_avds(self, run: unittest.mock.Mock) -> None:
        run.side_effect = [
            subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout="List of devices attached\nemulator-5554\tdevice\n",
            ),
            subprocess.CompletedProcess(args=[], returncode=0, stdout="another_avd\n"),
        ]

        serial = android_start_emulator._running_avd_serial("adb")

        self.assertIsNone(serial)


class ReadyDeviceSerialsTest(unittest.TestCase):
    @patch("android_start_emulator.subprocess.run")
    def test_includes_emulators_usb_and_wifi_devices(self, run: unittest.mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            args=[],
            returncode=0,
            stdout=(
                "List of devices attached\n"
                "emulator-5554\tdevice product:sdk\n"
                "R3CN30ABCDE\tdevice usb:1-1\n"
                "pixel.local:5555\tdevice product:pixel\n"
                "offline.local:5555\toffline\n"
            ),
        )

        serials = android_start_emulator._ready_device_serials("adb")

        self.assertEqual(
            ["emulator-5554", "R3CN30ABCDE", "pixel.local:5555"],
            serials,
        )


if __name__ == "__main__":
    unittest.main()
