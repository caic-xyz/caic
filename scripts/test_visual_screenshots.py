"""Unit tests for deterministic screenshot luminance comparison and render modes."""

import argparse
import unittest
from unittest.mock import patch

import visual_screenshots
from visual_screenshots import (
    MAX_LUMA_DELTA,
    MAX_LUMA_ERROR_PER_MILLION_PIXELS,
    luma_difference,
)


class LumaDifferenceTest(unittest.TestCase):
    def test_identical_pixels_are_accepted(self):
        difference = luma_difference(bytes((0, 128, 255)), bytes((0, 128, 255)))

        self.assertTrue(difference.acceptable)
        self.assertEqual(difference.maximum, 0)
        self.assertEqual(difference.total, 0)

    def test_observed_repeatability_bound_is_accepted(self):
        actual = bytes(1_000_000)
        expected = bytearray(actual)
        expected[: MAX_LUMA_ERROR_PER_MILLION_PIXELS // MAX_LUMA_DELTA] = bytes(
            [MAX_LUMA_DELTA] * (MAX_LUMA_ERROR_PER_MILLION_PIXELS // MAX_LUMA_DELTA),
        )

        difference = luma_difference(actual, expected)

        self.assertTrue(difference.acceptable)
        self.assertEqual(difference.total, MAX_LUMA_ERROR_PER_MILLION_PIXELS)

    def test_excessive_single_pixel_delta_is_rejected(self):
        difference = luma_difference(bytes((0,)), bytes((MAX_LUMA_DELTA + 1,)))

        self.assertFalse(difference.acceptable)

    def test_excessive_total_delta_is_rejected(self):
        actual = bytes(1_000_000)
        expected = bytearray(actual)
        expected[: MAX_LUMA_ERROR_PER_MILLION_PIXELS + 1] = bytes(
            [1] * (MAX_LUMA_ERROR_PER_MILLION_PIXELS + 1),
        )

        difference = luma_difference(actual, expected)

        self.assertFalse(difference.acceptable)

    def test_different_lengths_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "different lengths"):
            luma_difference(bytes((0,)), bytes((0, 0)))


class GenerateModeTest(unittest.TestCase):
    """The generate mode renders without the comparison precheck.

    Only the comparison modes decode images, so they require ffprobe/ffmpeg up
    front; the generate mode leaves that to each renderer, which needs ffmpeg on
    its own to encode the screenshots. The renderers are stubbed here, so these
    tests pin the mode split and not the renderer tooling.
    """

    def test_generate_renders_once_without_the_comparison_precheck(self):
        rendered: list[str] = []

        def render(directory):
            directory.mkdir(parents=True)
            rendered.append(str(directory))

        with (
            patch.object(visual_screenshots.shutil, "which", return_value=None),
            patch.dict(
                visual_screenshots.__dict__,
                {"render_frontend": render},
            ),
            patch.dict(
                visual_screenshots.__dict__,
                {
                    "parse_args": lambda: argparse.Namespace(
                        mode="generate",
                        platform="frontend",
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 0)
        self.assertEqual(len(rendered), 1)

    def test_check_still_requires_ffmpeg(self):
        with (
            patch.object(visual_screenshots.shutil, "which", return_value=None),
            patch.dict(
                visual_screenshots.__dict__,
                {
                    "parse_args": lambda: argparse.Namespace(
                        mode="check",
                        platform="frontend",
                    ),
                },
            ),
        ):
            self.assertEqual(visual_screenshots.main(), 1)


if __name__ == "__main__":
    unittest.main()
