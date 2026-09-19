#!/usr/bin/env python3
"""Precompress built assets so a static handler can serve them verbatim.

Each requested encoding is written beside its source file. Go servers embed
brotli-only output and delete the originals; Node servers keep the originals
because Hono's static handler falls back to them for a client that offers none
of the precompressed encodings. Silent on success; failures go to stderr and
exit nonzero.
"""

import argparse
import os
import subprocess
import sys
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from functools import partial
from pathlib import Path

# Suffixes produced by this script and by earlier runs, never compressed again.
COMPRESSION_SUFFIXES = (".br", ".gz", ".zst")


def encode_brotli(source: Path, destination: Path) -> None:
    subprocess.run(
        ["brotli", "--best", "--keep", "-o", str(destination), "--", str(source)],
        check=True,
        stdout=subprocess.DEVNULL,
    )


def encode_zstd(source: Path, destination: Path) -> None:
    subprocess.run(
        ["zstd", "-19", "--keep", "--no-progress", "-q", "-o", str(destination), "--", str(source)],
        check=True,
        stdout=subprocess.DEVNULL,
    )


def encode_gzip(source: Path, destination: Path) -> None:
    # -n keeps the name and timestamp out of the header so builds are reproducible.
    with destination.open("wb") as output:
        subprocess.run(
            ["gzip", "-9", "-n", "-c", "--", str(source)],
            check=True,
            stdout=output,
        )


# Ordered the way a server picks among available encodings.
ENCODERS: dict[str, tuple[str, Callable[[Path, Path], None]]] = {
    "br": (".br", encode_brotli),
    "zstd": (".zst", encode_zstd),
    "gzip": (".gz", encode_gzip),
}


def compress(source: Path, encodings: list[str], delete_original: bool) -> None:
    for name in encodings:
        suffix, encode = ENCODERS[name]
        encode(source, source.with_name(source.name + suffix))
    if delete_original:
        source.unlink()


def should_compress(path: Path, extensions: set[str]) -> bool:
    if path.is_symlink() or not path.is_file():
        return False
    suffix = path.suffix.lower()
    if suffix in COMPRESSION_SUFFIXES:
        return False
    return not extensions or suffix in extensions


def split_list(value: str) -> list[str]:
    return [item.strip() for item in value.split(",") if item.strip()]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dist", required=True, type=Path, help="Directory holding the built assets.")
    parser.add_argument("--encodings", default="br", help="Comma-separated encoding names: br, zstd, gzip.")
    parser.add_argument(
        "--extensions",
        default="",
        help="Comma-separated file suffixes to compress, for example .js,.css. Empty compresses every file.",
    )
    parser.add_argument(
        "--delete-originals",
        action="store_true",
        help="Remove each source file once every requested encoding is written.",
    )
    parser.add_argument("--jobs", type=int, default=os.cpu_count() or 1, help="Parallel compression workers.")
    arguments = parser.parse_args()

    encodings = split_list(arguments.encodings)
    unsupported = sorted(set(encodings) - set(ENCODERS))
    if unsupported:
        print(f"unsupported encodings: {', '.join(unsupported)}", file=sys.stderr)
        return 2
    if not encodings:
        print("no encodings requested", file=sys.stderr)
        return 2
    if not arguments.dist.is_dir():
        print(f"dist directory not found: {arguments.dist}", file=sys.stderr)
        return 1

    extensions = {name.lower() for name in split_list(arguments.extensions)}
    sources = [path for path in sorted(arguments.dist.rglob("*")) if should_compress(path, extensions)]
    compress_one = partial(compress, encodings=encodings, delete_original=arguments.delete_originals)
    try:
        with ThreadPoolExecutor(max_workers=max(1, arguments.jobs)) as executor:
            for _ in executor.map(compress_one, sources):
                pass
    except subprocess.CalledProcessError as error:
        print(f"compression failed: {error.cmd[0]} exited with status {error.returncode}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
