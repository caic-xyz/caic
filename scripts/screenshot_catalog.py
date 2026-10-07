"""Validate screenshot catalogs and publish managed directories with recoverable prior versions."""

import fcntl
import hashlib
import json
import pathlib
import re
import shutil
import tempfile
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass


@dataclass(frozen=True)
class CatalogSpec:
    producer: str
    platform: str
    suffixes: frozenset[str]
    expected_files: frozenset[str] | None


def validate_catalog(
    directory: pathlib.Path, spec: CatalogSpec, dimensions: Callable[[pathlib.Path], tuple[int, int]]
) -> dict:
    """Verify catalog provenance, exact image inventory, hashes, and decoded dimensions."""
    manifest = directory / "manifest.json"
    if directory.is_symlink() or manifest.is_symlink() or not manifest.is_file():
        raise RuntimeError(f"Missing or symlinked screenshot catalog: {directory}")
    if manifest.stat().st_size > 2_000_000:
        raise RuntimeError("Screenshot catalog exceeds 2 MB")
    catalog = json.loads(manifest.read_text(encoding="utf-8"))
    if not isinstance(catalog, dict) or any(
        catalog.get(key) != value
        for key, value in (("schemaVersion", 1), ("producer", spec.producer), ("platform", spec.platform))
    ):
        raise RuntimeError("Screenshot catalog has the wrong producer or schema")
    source = catalog.get("source")
    if not isinstance(source, dict) or any(
        not isinstance(source.get(key), str) or not re.fullmatch(pattern, source[key])
        for key, pattern in (("revision", r"[0-9a-f]{40}"), ("inputsSha256", r"[0-9a-f]{64}"))
    ):
        raise RuntimeError("Screenshot catalog provenance is invalid")
    scenarios = catalog.get("scenarios")
    if not isinstance(scenarios, list) or not scenarios:
        raise RuntimeError("Screenshot catalog has no scenes")
    names, files = set(), set()
    for scene in scenarios:
        if not isinstance(scene, dict):
            raise RuntimeError("Screenshot scene is not an object")
        name, filename = scene.get("name"), scene.get("file")
        if not isinstance(name, str) or not name or name in names:
            raise RuntimeError("Screenshot scene names must be unique and nonempty")
        if not isinstance(filename, str):
            raise RuntimeError("Screenshot file must be a relative path")
        relative = pathlib.PurePosixPath(filename)
        if (
            relative.is_absolute()
            or str(relative) != filename
            or ".." in relative.parts
            or "\\" in filename
            or relative.suffix not in spec.suffixes
            or filename in files
        ):
            raise RuntimeError(f"Invalid screenshot path: {filename}")
        path = directory / relative
        if any(parent.is_symlink() for parent in (path, *path.parents) if parent != directory.parent):
            raise RuntimeError(f"Symlinked screenshot path: {filename}")
        if not path.is_file() or path.stat().st_size > 16_000_000:
            raise RuntimeError(f"Missing or oversized screenshot: {filename}")
        if scene.get("sha256") != hashlib.sha256(path.read_bytes()).hexdigest():
            raise RuntimeError(f"Screenshot hash disagrees with catalog: {filename}")
        width, height = dimensions(path)
        if (
            type(scene.get("width")) is not int
            or type(scene.get("height")) is not int
            or (scene["width"], scene["height"]) != (width, height)
            or width <= 0
            or height <= 0
            or not isinstance(scene.get("description"), str)
            or not scene["description"]
        ):
            raise RuntimeError(f"Screenshot dimensions or description disagree with catalog: {filename}")
        names.add(name)
        files.add(filename)
    actual = set()
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise RuntimeError(f"Symlink in screenshot directory: {path.name}")
        if path.is_file() and path != manifest:
            actual.add(path.relative_to(directory).as_posix())
    if actual != files or (spec.expected_files is not None and files != spec.expected_files):
        raise RuntimeError("Screenshot catalog inventory disagrees with images")
    return catalog


def managed_paths(root: pathlib.Path, destination: pathlib.Path) -> pathlib.Path:
    """Refuse writes through symlinks or outside the canonical repository."""
    canonical = root.resolve(strict=True)
    if root != canonical or not destination.is_relative_to(canonical) or destination == canonical:
        raise RuntimeError("Screenshot destination must be inside the canonical repository")
    backup = destination.with_name(f".{destination.name}.previous")
    for path in (destination, backup):
        for ancestor in (path, *path.parents):
            if ancestor == canonical:
                break
            if ancestor.is_symlink():
                raise RuntimeError(f"Symlinked screenshot destination: {ancestor}")
    return backup


@contextmanager
def publication_lock(root: pathlib.Path, destination: pathlib.Path) -> Iterator[pathlib.Path]:
    """Reject concurrent writers before exchanging a managed screenshot catalog."""
    backup = managed_paths(root, destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    lock = destination.with_name(f".{destination.name}.publish.lock")
    if lock.is_symlink():
        raise RuntimeError("Symlinked screenshot publication lock")
    with lock.open("a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RuntimeError("Another screenshot publisher is active") from error
        yield backup


def recover_previous(destination: pathlib.Path, backup: pathlib.Path, validate: Callable[[pathlib.Path], dict]) -> None:
    """Restore a validated previous catalog while holding the publication lock."""
    if not destination.exists() and backup.exists():
        validate(backup)
        backup.rename(destination)


def recover_catalog(root: pathlib.Path, destination: pathlib.Path, validate: Callable[[pathlib.Path], dict]) -> None:
    """Recover an interrupted exchange only for an explicitly requested update."""
    with publication_lock(root, destination) as backup:
        recover_previous(destination, backup, validate)


def publish_catalog(
    source: pathlib.Path,
    root: pathlib.Path,
    destination: pathlib.Path,
    validate: Callable[[pathlib.Path], dict],
    validate_previous: Callable[[pathlib.Path], dict],
) -> None:
    """Stage a complete validated catalog before exchanging directories.

    The fixed previous directory survives process interruption and successful
    publication. A later update recovers it if interruption left no destination.
    Checks never perform recovery or any other mutation.
    Previous versions require integrity, while the incoming catalog can enforce
    a newer scene inventory.
    """
    with publication_lock(root, destination) as backup:
        recover_previous(destination, backup, validate_previous)
        with tempfile.TemporaryDirectory(prefix=f".{destination.name}-publish-", dir=destination.parent) as temporary:
            staged = pathlib.Path(temporary) / "catalog"
            shutil.copytree(source, staged, symlinks=True)
            validate(staged)
            # Only discard an older backup while a validated current catalog is
            # still intact. Once that catalog moves, retain it through every exit.
            if destination.exists():
                validate_previous(destination)
                if backup.exists():
                    validate_previous(backup)
                    # Clean obsolete versions under the staging directory.
                    # Interrupted cleanup must never leave the fixed recovery
                    # path partially deleted and block a later update.
                    backup.rename(pathlib.Path(temporary) / "obsolete")
                destination.rename(backup)
            try:
                staged.rename(destination)
            except OSError:
                if backup.exists() and not destination.exists():
                    backup.rename(destination)
                raise
