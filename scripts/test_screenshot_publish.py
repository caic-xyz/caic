"""Regression tests for screenshot image integrity and interruption-safe publication."""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import screenshot_publish
from screenshot_publish import publish_images, recover_images, validate_images


class ImagePublicationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.old = self.root / "captures" / "android"
        self.new = self.root / "new"
        self.write_images(self.old, b"old scene")
        self.write_images(self.new, b"new scene")

    def write_images(self, directory, pixels):
        directory.mkdir(parents=True)
        (directory / "scene.webp").write_bytes(pixels)

    def validate(self, directory):
        return validate_images(directory, frozenset({"scene.webp"}), frozenset({".webp"}), lambda _path: (10, 20))

    def validate_previous(self, directory):
        return validate_images(directory, None, frozenset({".webp"}), lambda _path: (10, 20))

    def snapshot(self, directory):
        return {path.name: path.read_bytes() for path in directory.iterdir()}

    def test_failed_copy_preserves_complete_previous_images(self):
        before = self.snapshot(self.old)
        with mock.patch.object(screenshot_publish.shutil, "copytree", side_effect=OSError("disk full")):
            with self.assertRaisesRegex(OSError, "disk full"):
                publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_failed_exchange_rolls_back_without_mixing_images(self):
        before = self.snapshot(self.old)
        rename = Path.rename

        def fail_new(path, target):
            if path.name == "images":
                raise OSError("exchange failed")
            return rename(path, target)

        with mock.patch.object(Path, "rename", fail_new):
            with self.assertRaisesRegex(OSError, "exchange failed"):
                publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_interrupted_exchange_preserves_recoverable_prior_images(self):
        before = self.snapshot(self.old)
        rename = Path.rename

        def interrupt(path, target):
            if path.name == "images":
                raise KeyboardInterrupt()
            return rename(path, target)

        with mock.patch.object(Path, "rename", interrupt):
            with self.assertRaises(KeyboardInterrupt):
                publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.assertFalse(self.old.exists())
        backup = self.old.with_name(".android.previous")
        self.assertEqual(self.snapshot(backup), before)
        # A read-only check fails without consuming the recovery copy.
        with self.assertRaisesRegex(RuntimeError, "Missing"):
            self.validate(self.old)
        self.assertEqual(self.snapshot(backup), before)
        recover_images(self.root, self.old, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_success_keeps_previous_complete_images(self):
        before = self.snapshot(self.old)
        publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), self.snapshot(self.new))
        self.assertEqual(self.snapshot(self.old.with_name(".android.previous")), before)
        publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.validate(self.old)

    def test_new_image_inventory_preserves_prior_valid_version(self):
        before = self.snapshot(self.old)
        (self.new / "resources.webp").write_bytes(b"resource charts")
        publish_images(
            self.new,
            self.root,
            self.old,
            lambda path: validate_images(
                path, frozenset({"scene.webp", "resources.webp"}), frozenset({".webp"}), lambda _path: (10, 20)
            ),
            self.validate_previous,
        )
        validate_images(
            self.old, frozenset({"scene.webp", "resources.webp"}), frozenset({".webp"}), lambda _p: (10, 20)
        )
        self.assertEqual(self.snapshot(self.old.with_name(".android.previous")), before)

    def test_inventory_disagreement_fails_read_only(self):
        before = self.snapshot(self.old)
        with self.assertRaisesRegex(RuntimeError, "inventory"):
            validate_images(
                self.old, frozenset({"scene.webp", "missing.webp"}), frozenset({".webp"}), lambda _path: (10, 20)
            )
        self.assertEqual(self.snapshot(self.old), before)
        (self.old / "unexpected.webp").write_bytes(b"unexpected")
        with self.assertRaisesRegex(RuntimeError, "inventory"):
            self.validate(self.old)

    def test_non_image_file_fails_read_only(self):
        before = self.snapshot(self.old)
        (self.old / "notes.txt").write_text("not an image")
        with self.assertRaisesRegex(RuntimeError, "Unexpected file"):
            self.validate(self.old)
        (self.old / "notes.txt").unlink()
        self.assertEqual(self.snapshot(self.old), before)

    def test_nonpositive_dimensions_fail_read_only(self):
        with self.assertRaisesRegex(RuntimeError, "dimensions"):
            validate_images(self.new, frozenset({"scene.webp"}), frozenset({".webp"}), lambda _path: (0, 20))
        with self.assertRaisesRegex(RuntimeError, "dimensions"):
            validate_images(self.new, frozenset({"scene.webp"}), frozenset({".webp"}), lambda _path: (10, -1))

    def test_concurrent_publisher_is_refused_without_changing_images(self):
        before = self.snapshot(self.old)
        with screenshot_publish.publication_lock(self.root, self.old):
            with self.assertRaisesRegex(RuntimeError, "Another screenshot publisher"):
                publish_images(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_symlinked_ancestor_refuses_external_writes(self):
        external = self.root / "external"
        external.mkdir()
        (external / "sentinel").write_bytes(b"unchanged")
        (self.root / "linked").symlink_to(external, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "Symlinked"):
            publish_images(self.new, self.root, self.root / "linked" / "android", self.validate, self.validate)
        self.assertEqual(self.snapshot(external), {"sentinel": b"unchanged"})


if __name__ == "__main__":
    unittest.main()
