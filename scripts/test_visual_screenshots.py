"""Unit tests for deterministic screenshot luminance comparison."""

import unittest

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


if __name__ == "__main__":
    unittest.main()
