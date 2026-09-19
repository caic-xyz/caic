#!/usr/bin/env python3
"""Run every Python unit-test script and propagate the first failure."""

import subprocess
import sys
from collections.abc import Iterable
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent


def discover_test_files(root_dir: Path) -> list[Path]:
    """Return tracked and unignored Python test scripts throughout root_dir."""
    result = subprocess.run(
        [
            "git",
            "-C",
            str(root_dir),
            "ls-files",
            "-z",
            "--cached",
            "--others",
            "--exclude-standard",
            "--",
            ":(glob)**/test_*.py",
        ],
        capture_output=True,
        check=True,
    )
    relative_paths = result.stdout.decode().split("\0")
    return [root_dir / path for path in sorted(relative_paths) if path]


def run_test_files(test_files: Iterable[Path]) -> int:
    """Run test_files in order and return the first nonzero exit status."""
    for test_file in test_files:
        result = subprocess.run([sys.executable, str(test_file)], check=False)
        if result.returncode != 0:
            return result.returncode
    return 0


def main() -> int:
    test_files = discover_test_files(ROOT_DIR)
    if not test_files:
        print("No Python unit-test scripts found in the repository", file=sys.stderr)
        return 1
    return run_test_files(test_files)


if __name__ == "__main__":
    sys.exit(main())
