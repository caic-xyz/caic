"""Unit tests for fail-closed Python unit-test execution."""

import subprocess
import unittest
from pathlib import Path
from unittest import mock

from run_python_tests import discover_test_files, run_test_files


class DiscoverTestFilesTest(unittest.TestCase):
    @mock.patch("run_python_tests.subprocess.run")
    def test_includes_nested_unignored_test_suites(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            args=[],
            returncode=0,
            stdout=(b"scripts/test_run_python_tests.py\0backend/internal/agent/relay/test_relay_v2.py\0"),
        )

        tests = discover_test_files(Path("/repo"))

        self.assertEqual(
            tests,
            [
                Path("/repo/backend/internal/agent/relay/test_relay_v2.py"),
                Path("/repo/scripts/test_run_python_tests.py"),
            ],
        )
        self.assertIn("--exclude-standard", run.call_args.args[0])


class RunTestFilesTest(unittest.TestCase):
    @mock.patch("run_python_tests.subprocess.run")
    def test_propagates_failure_and_stops(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(args=[], returncode=7)

        result = run_test_files([Path("scripts/test_failure.py"), Path("scripts/test_later.py")])

        self.assertEqual(result, 7)
        run.assert_called_once()

    @mock.patch("run_python_tests.subprocess.run")
    def test_returns_success_after_every_test_passes(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(args=[], returncode=0)

        result = run_test_files([Path("scripts/test_one.py"), Path("scripts/test_two.py")])

        self.assertEqual(result, 0)
        self.assertEqual(run.call_count, 2)


if __name__ == "__main__":
    unittest.main()
