#!/usr/bin/env python3
"""Tests for Android E2E runner readiness helpers."""

import unittest
import urllib.error
from unittest import mock

from scripts import android_e2e


class WaitForBackendTest(unittest.TestCase):
    def test_timeout_retries(self):
        calls = 0

        def urlopen(_url, timeout):
            nonlocal calls
            calls += 1
            self.assertEqual(timeout, 2)
            if calls == 1:
                raise TimeoutError("timed out")
            return object()

        with (
            mock.patch.object(android_e2e.urllib.request, "urlopen", side_effect=urlopen),
            mock.patch.object(android_e2e.time, "monotonic", side_effect=[0, 0, 1]),
            mock.patch.object(android_e2e.time, "sleep") as sleep,
        ):
            self.assertTrue(android_e2e.wait_for_backend(1234))

        self.assertEqual(calls, 2)
        sleep.assert_called_once_with(0.5)

    def test_url_error_retries_until_deadline(self):
        with (
            mock.patch.object(
                android_e2e.urllib.request,
                "urlopen",
                side_effect=urllib.error.URLError("connection refused"),
            ) as urlopen,
            mock.patch.object(android_e2e.time, "monotonic", side_effect=[0, 0, 31]),
            mock.patch.object(android_e2e.time, "sleep") as sleep,
        ):
            self.assertFalse(android_e2e.wait_for_backend(1234))

        urlopen.assert_called_once()
        sleep.assert_called_once_with(0.5)


if __name__ == "__main__":
    unittest.main()
