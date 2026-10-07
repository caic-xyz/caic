"""Regression tests for screenshot catalog integrity and interruption-safe publication."""

import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import screenshot_catalog
from screenshot_catalog import CatalogSpec, publish_catalog, recover_catalog, validate_catalog


class CatalogPublicationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.old = self.root / "captures" / "android"
        self.new = self.root / "new"
        self.spec = CatalogSpec("demo", "android", frozenset({".webp"}), frozenset({"scene.webp"}))
        self.write_catalog(self.old, b"old scene")
        self.write_catalog(self.new, b"new scene")

    def write_catalog(self, directory, pixels):
        directory.mkdir(parents=True)
        (directory / "scene.webp").write_bytes(pixels)
        catalog = {
            "schemaVersion": 1,
            "producer": "demo",
            "platform": "android",
            "source": {"revision": "a" * 40, "inputsSha256": "b" * 64},
            "scenarios": [
                {
                    "name": "scene",
                    "file": "scene.webp",
                    "width": 10,
                    "height": 20,
                    "sha256": hashlib.sha256(pixels).hexdigest(),
                    "description": "A hosted demo.",
                }
            ],
        }
        (directory / "manifest.json").write_text(json.dumps(catalog))

    def validate(self, directory):
        return validate_catalog(directory, self.spec, lambda _path: (10, 20))

    def snapshot(self, directory):
        return {path.name: path.read_bytes() for path in directory.iterdir()}

    def test_failed_copy_preserves_complete_previous_catalog(self):
        before = self.snapshot(self.old)
        with mock.patch.object(screenshot_catalog.shutil, "copytree", side_effect=OSError("disk full")):
            with self.assertRaisesRegex(OSError, "disk full"):
                publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_failed_exchange_rolls_back_without_mixing_catalogs(self):
        before = self.snapshot(self.old)
        rename = Path.rename

        def fail_new(path, target):
            if path.name == "catalog":
                raise OSError("exchange failed")
            return rename(path, target)

        with mock.patch.object(Path, "rename", fail_new):
            with self.assertRaisesRegex(OSError, "exchange failed"):
                publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_interrupted_exchange_preserves_recoverable_prior_catalog(self):
        before = self.snapshot(self.old)
        rename = Path.rename

        def interrupt(path, target):
            if path.name == "catalog":
                raise KeyboardInterrupt()
            return rename(path, target)

        with mock.patch.object(Path, "rename", interrupt):
            with self.assertRaises(KeyboardInterrupt):
                publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.assertFalse(self.old.exists())
        backup = self.old.with_name(".android.previous")
        self.assertEqual(self.snapshot(backup), before)
        # A read-only check fails without consuming the recovery copy.
        with self.assertRaisesRegex(RuntimeError, "Missing"):
            self.validate(self.old)
        self.assertEqual(self.snapshot(backup), before)
        recover_catalog(self.root, self.old, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_success_keeps_previous_complete_catalog(self):
        before = self.snapshot(self.old)
        publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), self.snapshot(self.new))
        self.assertEqual(self.snapshot(self.old.with_name(".android.previous")), before)
        publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.validate(self.old)

    def test_new_scene_inventory_preserves_prior_valid_version(self):
        before = self.snapshot(self.old)
        pixels = b"resource charts"
        (self.new / "resources.webp").write_bytes(pixels)
        manifest = self.new / "manifest.json"
        catalog = json.loads(manifest.read_text())
        catalog["scenarios"].append(
            {
                **catalog["scenarios"][0],
                "name": "resources",
                "file": "resources.webp",
                "sha256": hashlib.sha256(pixels).hexdigest(),
            }
        )
        manifest.write_text(json.dumps(catalog))
        self.spec = CatalogSpec("demo", "android", frozenset({".webp"}), frozenset({"scene.webp", "resources.webp"}))
        previous_spec = CatalogSpec("demo", "android", frozenset({".webp"}), None)
        publish_catalog(
            self.new,
            self.root,
            self.old,
            self.validate,
            lambda path: validate_catalog(path, previous_spec, lambda _path: (10, 20)),
        )
        self.validate(self.old)
        self.assertEqual(self.snapshot(self.old.with_name(".android.previous")), before)

    def test_tampered_hash_dimensions_and_inventory_fail_read_only(self):
        manifest = self.old / "manifest.json"
        original = json.loads(manifest.read_text())
        for key, value, error in (("sha256", "c" * 64, "hash"), ("width", 11, "dimensions")):
            with self.subTest(key=key):
                catalog = json.loads(json.dumps(original))
                catalog["scenarios"][0][key] = value
                manifest.write_text(json.dumps(catalog))
                before = self.snapshot(self.old)
                with self.assertRaisesRegex(RuntimeError, error):
                    self.validate(self.old)
                self.assertEqual(self.snapshot(self.old), before)
        manifest.write_text(json.dumps(original))
        (self.old / "unexpected.webp").write_bytes(b"unexpected")
        with self.assertRaisesRegex(RuntimeError, "inventory"):
            self.validate(self.old)

    def test_concurrent_publisher_is_refused_without_changing_catalog(self):
        before = self.snapshot(self.old)
        with screenshot_catalog.publication_lock(self.root, self.old):
            with self.assertRaisesRegex(RuntimeError, "Another screenshot publisher"):
                publish_catalog(self.new, self.root, self.old, self.validate, self.validate)
        self.assertEqual(self.snapshot(self.old), before)

    def test_symlinked_ancestor_refuses_external_writes(self):
        external = self.root / "external"
        external.mkdir()
        (external / "sentinel").write_bytes(b"unchanged")
        (self.root / "linked").symlink_to(external, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "Symlinked"):
            publish_catalog(self.new, self.root, self.root / "linked" / "android", self.validate, self.validate)
        self.assertEqual(self.snapshot(external), {"sentinel": b"unchanged"})


if __name__ == "__main__":
    unittest.main()
