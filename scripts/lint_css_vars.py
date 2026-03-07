#!/usr/bin/env python3
# Lint for CSS custom properties and selectors in frontend/src.
import re
import subprocess
import sys
from pathlib import Path

_VAR_DEF_RE = re.compile(r"(--[\w-]+)\s*:")
_VAR_USE_RE = re.compile(r"var\((--[\w-]+)")
_CLASS_DEF_RE = re.compile(r"\.([a-zA-Z][\w-]*)")
_IMPORT_RE = re.compile(r'import\s+(\w+)\s+from\s+["\']([^"\']+\.module\.css)["\']')
_CSS_COMMENT_RE = re.compile(r"/\*.*?\*/", re.DOTALL)
_CSS_GLOBAL_RE = re.compile(r":global\([^)]*\)")


def check_css_vars(css_files: list[str], source_files: list[str]) -> list[tuple[str, int, str]]:
    """Report CSS custom properties used but not defined anywhere."""
    defined: set[str] = set()
    for path in css_files:
        try:
            for m in _VAR_DEF_RE.finditer(Path(path).read_text()):
                defined.add(m.group(1))
        except OSError:
            pass

    errors: list[tuple[str, int, str]] = []
    for path in source_files:
        try:
            text = Path(path).read_text()
            for m in _VAR_USE_RE.finditer(text):
                var = m.group(1)
                if var not in defined:
                    errors.append((path, text[: m.start()].count("\n") + 1, var))
        except OSError:
            pass
    return errors


def extract_css_classes(text: str) -> set[str]:
    cleaned = _CSS_GLOBAL_RE.sub("", _CSS_COMMENT_RE.sub("", text))
    return {m.group(1) for m in _CLASS_DEF_RE.finditer(cleaned)}


def find_used_classes(ts_text: str, alias: str) -> tuple[set[str], int]:
    """Return (used_class_names, dynamic_access_line).

    dynamic_access_line is non-zero when a dynamic bracket expression like
    styles[expr()] is found. Use an explicit Record<Variant, string> map instead.
    """
    m = re.search(r"\b" + re.escape(alias) + r"\[(?![\"'])", ts_text)
    if m:
        return set(), ts_text[: m.start()].count("\n") + 1
    used: set[str] = set()
    for m in re.finditer(r"\b" + re.escape(alias) + r"\.([\w]+)", ts_text):
        used.add(m.group(1))
    for m in re.finditer(r"\b" + re.escape(alias) + r"\[[\"']([\w-]+)[\"']\]", ts_text):
        used.add(m.group(1))
    return used, 0


def check_unused_selectors(files: list[str]) -> list[str]:
    """Report CSS module class selectors not referenced in any TS/TSX file."""
    module_css = [f for f in files if f.endswith(".module.css")]
    ts_files = [f for f in files if f.endswith((".ts", ".tsx"))]

    ts_texts: dict[str, str] = {}
    for path in ts_files:
        try:
            ts_texts[path] = Path(path).read_text()
        except OSError:
            pass

    errors: list[str] = []
    for css_path in module_css:
        try:
            css_text = Path(css_path).read_text()
        except OSError:
            continue

        defined_classes = extract_css_classes(css_text)
        if not defined_classes:
            continue

        css_resolved = Path(css_path).resolve()
        importers: list[tuple[str, str]] = []  # (ts_path, alias)
        for ts_path, ts_text in ts_texts.items():
            for im in _IMPORT_RE.finditer(ts_text):
                alias, import_path = im.group(1), im.group(2)
                if (Path(ts_path).parent / import_path).resolve() == css_resolved:
                    importers.append((ts_path, alias))

        if not importers:
            for cls in sorted(defined_classes):
                errors.append(f"  {css_path}: .{cls} (not imported)")
            continue

        all_used: set[str] = set()
        skip = False
        for ts_path, alias in importers:
            used, dyn_line = find_used_classes(ts_texts[ts_path], alias)
            if dyn_line:
                errors.append(
                    f"  {ts_path}:{dyn_line}: dynamic CSS module access `{alias}[...]`"
                    " — use an explicit Record<Variant, string> map instead"
                )
                skip = True
                break
            all_used |= used

        if not skip:
            for cls in sorted(defined_classes):
                if cls not in all_used:
                    errors.append(f"  {css_path}: .{cls}")

    return errors


def main() -> int:
    try:
        files = subprocess.check_output(["git", "ls-files", "frontend/src"], text=True).splitlines()
    except subprocess.CalledProcessError as e:
        print(f"Error running git: {e}", file=sys.stderr)
        return 1

    css_files = [f for f in files if f.endswith(".css")]
    source_files = [f for f in files if f.endswith((".css", ".ts", ".tsx"))]

    rc = 0

    var_errors = check_css_vars(css_files, source_files)
    if var_errors:
        print("Error: undefined CSS custom properties:")
        for path, line, var in sorted(var_errors):
            print(f"  {path}:{line}: {var}")
        rc = 1

    selector_errors = check_unused_selectors(files)
    if selector_errors:
        print("Error: CSS module selector issues:")
        for msg in selector_errors:
            print(msg)
        rc = 1

    return rc


if __name__ == "__main__":
    sys.exit(main())
