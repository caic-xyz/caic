#!/usr/bin/env python3
# Lint for unexpected binary or executable files in the repository.
import os
import stat
import subprocess
import sys

ALLOWED_BINARY_EXT = {".avif", ".br", ".gif", ".ico", ".jar", ".jpg", ".png", ".svg", ".webp", ".zst"}


def is_binary(file_path):
    """Simple binary detection by checking for null bytes in the first 1024 bytes."""
    if not os.path.isfile(file_path):
        return False
    try:
        with open(file_path, "rb") as f:
            chunk = f.read(1024)
            return b"\0" in chunk
    except Exception:
        return False


def has_shebang(file_path):
    """Check if file starts with a shebang line."""
    try:
        with open(file_path, "rb") as f:
            return f.read(2) == b"#!"
    except Exception:
        return False


def is_executable(file_path):
    """Check if file has executable bit set."""
    try:
        return os.stat(file_path).st_mode & stat.S_IXUSR != 0
    except Exception:
        return False


def main():
    try:
        files = subprocess.check_output(["git", "ls-files"], text=True).splitlines()
    except subprocess.CalledProcessError as e:
        print(f"Error running git: {e}", file=sys.stderr)
        return 1

    unexpected_binaries = []
    unexpected_executables = []

    for f in files:
        ext = os.path.splitext(f)[1].lower()
        if is_binary(f) and ext not in ALLOWED_BINARY_EXT:
            unexpected_binaries.append(f)
        if is_executable(f) and not has_shebang(f):
            unexpected_executables.append(f)

    rc = 0
    if unexpected_binaries:
        print("Error: unexpected binary files in repository:")
        for b in unexpected_binaries:
            print(f"  {b}")
        rc = 1
    if unexpected_executables:
        print("Error: unexpected executable files in repository:")
        for e in unexpected_executables:
            print(f"  {e}")
        rc = 1
    return rc


if __name__ == "__main__":
    sys.exit(main())
