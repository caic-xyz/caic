"""Unit tests for Go build-tag discovery used by the compile-check lint."""

import tempfile
import unittest
from pathlib import Path

from lint_build_tags import TOOLCHAIN_TAGS, custom_tags

# A stand-in for the GOOS and GOARCH values the real toolchain reports.
PLATFORMS = frozenset({"linux", "darwin", "amd64", "arm64"})
EXCLUDED = TOOLCHAIN_TAGS | PLATFORMS


class CustomTagsTest(unittest.TestCase):
    def tags_for(self, *headers: str) -> list[str]:
        """Return the custom tags discovered across one file per header."""
        with tempfile.TemporaryDirectory() as directory:
            paths = []
            for i, header in enumerate(headers):
                path = Path(directory) / f"f{i}.go"
                path.write_text(f"{header}\n\npackage main\n", encoding="utf-8")
                paths.append(str(path))
            return custom_tags(paths, EXCLUDED)

    def test_finds_a_plain_tag(self) -> None:
        self.assertEqual(self.tags_for("//go:build smoke"), ["smoke"])

    def test_finds_the_tag_behind_a_negation(self) -> None:
        # !e2e guards code that only compiles when e2e is absent, so e2e is
        # still the tag that must be vetted separately.
        self.assertEqual(self.tags_for("//go:build !e2e"), ["e2e"])

    def test_splits_compound_expressions(self) -> None:
        self.assertEqual(
            self.tags_for("//go:build (smoke || e2e) && !real_task_logs"),
            ["e2e", "real_task_logs", "smoke"],
        )

    def test_drops_platform_and_toolchain_terms(self) -> None:
        self.assertEqual(self.tags_for("//go:build linux && !race && cgo"), [])

    def test_drops_go_version_terms(self) -> None:
        self.assertEqual(self.tags_for("//go:build go1.24 && smoke"), ["smoke"])

    def test_ignores_a_build_line_that_is_not_a_constraint(self) -> None:
        self.assertEqual(self.tags_for("// go:build smoke"), [])

    def test_deduplicates_across_files(self) -> None:
        self.assertEqual(self.tags_for("//go:build smoke", "//go:build smoke"), ["smoke"])


if __name__ == "__main__":
    unittest.main()
