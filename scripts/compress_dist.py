#!/usr/bin/env python3
"""Precompress frontend assets with brotli and delete originals.

Produces a .br file for each asset in the dist directory at maximum
compression. The original file is removed afterward so only .br files
are embedded into the Go binary, minimizing binary size. The server
lazily transcodes to other encodings at runtime.

Called as a postbuild step by the frontend build script.
"""

import subprocess
import sys
from pathlib import Path


def compress_file(path: Path) -> None:
    s = str(path)
    subprocess.run(["brotli", "--best", "--keep", s, "-o", s + ".br"], check=True)
    path.unlink()


def main() -> None:
    dist = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("backend/frontend/dist")
    if not dist.is_dir():
        print(f"dist directory not found: {dist}", file=sys.stderr)
        sys.exit(1)

    for path in sorted(dist.rglob("*")):
        if path.is_file() and path.suffix != ".br":
            compress_file(path)


if __name__ == "__main__":
    main()
