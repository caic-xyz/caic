#!/usr/bin/env python3
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

"""Compile-check every Go build tag that guards code the default build skips."""

import re
import subprocess
import sys

# Constraint terms the toolchain defines itself. Vetting these selects a
# platform or a toolchain mode rather than opting extra source into the build.
TOOLCHAIN_TAGS = frozenset(
    {"cgo", "gc", "gccgo", "msan", "asan", "purego", "race", "unix"},
)
GO_VERSION_TAG = re.compile(r"^go1\.\d+$")
BUILD_LINE = re.compile(r"^//go:build (.+)$", re.MULTILINE)
# && || ! ( ) separate the terms of a build expression; nothing else does.
TERM_SEPARATOR = re.compile(r"[()!]|&&|\|\|")


def platform_tags() -> frozenset[str]:
    """Return every GOOS and GOARCH value this toolchain knows."""
    raw = subprocess.check_output(("go", "tool", "dist", "list"), text=True)
    values: set[str] = set()
    for line in raw.split():
        goos, _, goarch = line.partition("/")
        values.update((goos, goarch))
    return frozenset(values)


def tracked_go_files() -> list[str]:
    """Return all Git-tracked Go source paths."""
    raw = subprocess.check_output(("git", "ls-files", "-z", "*.go"))
    return [path.decode() for path in raw.split(b"\0") if path]


def custom_tags(paths: list[str], excluded: frozenset[str]) -> list[str]:
    """Return the sorted build tags in paths that opt extra source into a build."""
    found: set[str] = set()
    for path in paths:
        with open(path, encoding="utf-8") as f:
            header = f.read(4096)
        for expression in BUILD_LINE.findall(header):
            for term in TERM_SEPARATOR.sub(" ", expression).split():
                if term not in excluded and not GO_VERSION_TAG.match(term):
                    found.add(term)
    return sorted(found)


def main() -> int:
    """Vet each custom build tag and report the ones that no longer compile."""
    tags = custom_tags(tracked_go_files(), TOOLCHAIN_TAGS | platform_tags())
    if not tags:
        print("No custom Go build tags found; nothing to compile-check.", file=sys.stderr)
        return 1
    # One run per tag: tags compose, so a single combined run would enable e2e
    # and never compile the files a negated constraint such as !e2e guards.
    broken: list[str] = []
    for tag in tags:
        if subprocess.run(("go", "vet", f"-tags={tag}", "./..."), check=False).returncode:
            broken.append(tag)
    if broken:
        print(f"Build tags that no longer compile: {', '.join(broken)}", file=sys.stderr)
    return int(bool(broken))


if __name__ == "__main__":
    sys.exit(main())
