"""Validate screenshot image inventories and publish managed directories with recoverable prior versions."""

import fcntl
import pathlib
import shutil
import tempfile
from collections.abc import Callable, Iterator
from contextlib import contextmanager


def validate_images(
    directory: pathlib.Path,
    expected_files: frozenset[str] | None,
    suffixes: frozenset[str],
    dimensions: Callable[[pathlib.Path], tuple[int, int]],
) -> None:
    """Verify the exact image inventory and positive image dimensions.

    A None expected inventory accepts any nonempty set of images, which lets a
    publication validate a previous version whose scene list predates the
    current producer.
    """
    if directory.is_symlink() or not directory.is_dir():
        raise RuntimeError(f"Missing or symlinked screenshot directory: {directory}")
    actual: set[str] = set()
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise RuntimeError(f"Symlink in screenshot directory: {path.name}")
        if path.is_dir():
            continue
        if not path.is_file() or path.suffix not in suffixes:
            raise RuntimeError(f"Unexpected file in screenshot directory: {path.name}")
        actual.add(path.relative_to(directory).as_posix())
    if expected_files is not None and actual != expected_files:
        raise RuntimeError("Screenshot image inventory disagrees with the expected images")
    if not actual:
        raise RuntimeError("Screenshot directory has no images")
    for name in sorted(actual):
        width, height = dimensions(directory / name)
        if width <= 0 or height <= 0:
            raise RuntimeError(f"Screenshot dimensions are invalid: {name}")


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
    """Reject concurrent writers before exchanging a managed screenshot directory."""
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


def recover_previous(destination: pathlib.Path, backup: pathlib.Path, validate: Callable[[pathlib.Path], None]) -> None:
    """Restore a validated previous image directory while holding the publication lock."""
    if not destination.exists() and backup.exists():
        validate(backup)
        backup.rename(destination)


def recover_images(root: pathlib.Path, destination: pathlib.Path, validate: Callable[[pathlib.Path], None]) -> None:
    """Recover an interrupted exchange only for an explicitly requested update."""
    with publication_lock(root, destination) as backup:
        recover_previous(destination, backup, validate)


def publish_images(
    source: pathlib.Path,
    root: pathlib.Path,
    destination: pathlib.Path,
    validate: Callable[[pathlib.Path], None],
    validate_previous: Callable[[pathlib.Path], None],
) -> None:
    """Stage a complete validated image directory before exchanging directories.

    The fixed previous directory survives process interruption and successful
    publication. A later update recovers it if interruption left no destination.
    Checks never perform recovery or any other mutation.
    Previous versions require valid images, while the incoming directory can
    enforce a newer image inventory.
    """
    with publication_lock(root, destination) as backup:
        recover_previous(destination, backup, validate_previous)
        with tempfile.TemporaryDirectory(prefix=f".{destination.name}-publish-", dir=destination.parent) as temporary:
            staged = pathlib.Path(temporary) / "images"
            shutil.copytree(source, staged, symlinks=True)
            validate(staged)
            # Only discard an older backup while a validated current directory
            # is still intact. Once that directory moves, retain it through
            # every exit.
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
