#!/usr/bin/env python3
"""Lint CSS variables, module selectors, and raw colors in frontend sources."""

import argparse
import re
import subprocess
import sys
from pathlib import Path

_VAR_DEF_RE = re.compile(r"(--[\w-]+)\s*:")
_VAR_USE_RE = re.compile(r"var\((--[\w-]+)")
_CLASS_DEF_RE = re.compile(r"\.([a-zA-Z][\w-]*)")
_IMPORT_RE = re.compile(r'import\s+(\w+)\s+from\s+["\']([^"\']+\.module\.css)["\']')
_CSS_GLOBAL_RE = re.compile(r":global\([^)]*\)")
_HEX_COLOR_RE = re.compile(r"(?<!&)#[0-9a-fA-F]{3,8}\b")
_FUNC_COLOR_RE = re.compile(r"\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(([^)]*)\)")
_NAMED_COLORS = frozenset(
    """
    aliceblue antiquewhite aqua aquamarine azure beige bisque black
    blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse chocolate
    coral cornflowerblue cornsilk crimson cyan darkblue darkcyan darkgoldenrod
    darkgray darkgrey darkgreen darkkhaki darkmagenta darkolivegreen darkorange darkorchid
    darkred darksalmon darkseagreen darkslateblue darkslategray darkslategrey darkturquoise darkviolet
    deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite forestgreen
    fuchsia gainsboro ghostwhite gold goldenrod gray grey green
    greenyellow honeydew hotpink indianred indigo ivory khaki lavender
    lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan lightgoldenrodyellow lightgray
    lightgrey lightgreen lightpink lightsalmon lightseagreen lightskyblue lightslategray lightslategrey
    lightsteelblue lightyellow lime limegreen linen magenta maroon mediumaquamarine
    mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue mediumspringgreen
    mediumturquoise mediumvioletred
    midnightblue mintcream mistyrose moccasin navajowhite navy oldlace olive
    olivedrab orange orangered orchid palegoldenrod palegreen paleturquoise palevioletred
    papayawhip peachpuff peru pink plum powderblue purple red
    rebeccapurple rosybrown royalblue saddlebrown salmon sandybrown seagreen seashell
    sienna silver skyblue slateblue slategray slategrey snow springgreen
    steelblue tan teal thistle tomato turquoise violet wheat
    white whitesmoke yellow yellowgreen
    """.split()
)
_NAMED_COLOR_RE = re.compile(rf"(?<![-\w.])({'|'.join(sorted(_NAMED_COLORS))})(?![-\w])", re.IGNORECASE)
_PROP_DEF_RE = re.compile(r"--[A-Za-z][\w-]*\s*:[^;{}]*;")


def _strip_comments(text: str, *, line_comments: bool = False, html_comments: bool = False) -> str:
    """Blank comments while preserving quoted strings and line numbers."""
    chars = list(text)
    quote: str | None = None
    i = 0
    while i < len(chars):
        if quote is not None:
            if chars[i] == "\\":
                i += 2
                continue
            if chars[i] == quote:
                quote = None
            i += 1
            continue

        if chars[i] in {'"', "'", "`"}:
            quote = chars[i]
            i += 1
            continue

        marker_length = 0
        end_marker = ""
        if html_comments and text.startswith("<!--", i):
            marker_length = 4
            end_marker = "-->"
        elif text.startswith("/*", i):
            marker_length = 2
            end_marker = "*/"
        elif line_comments and text.startswith("//", i):
            end = text.find("\n", i)
            if end < 0:
                end = len(chars)
            for j in range(i, end):
                chars[j] = " "
            i = end
            continue

        if marker_length:
            end = text.find(end_marker, i + marker_length)
            if end < 0:
                end = len(chars)
            else:
                end += len(end_marker)
            for j in range(i, end):
                if chars[j] != "\n":
                    chars[j] = " "
            i = end
            continue

        i += 1
    return "".join(chars)


def _read_source(path: str | Path) -> str:
    source_path = Path(path)
    text = source_path.read_text(encoding="utf-8")
    suffix = source_path.suffix
    return _strip_comments(
        text,
        line_comments=suffix in {".html", ".ts", ".tsx"},
        html_comments=suffix == ".html",
    )


def check_css_vars(variable_files: list[str], source_files: list[str], token_file: str) -> list[tuple[str, int, str]]:
    """Report variables unavailable from the shared token file or owning source file."""
    variable_texts = {Path(path).resolve(): _read_source(path) for path in variable_files}
    token_path = Path(token_file).resolve()
    token_text = variable_texts.get(token_path)
    if token_text is None:
        token_text = _read_source(token_file)
    shared = {m.group(1) for m in _VAR_DEF_RE.finditer(token_text)}
    local = {path: {m.group(1) for m in _VAR_DEF_RE.finditer(text)} for path, text in variable_texts.items()}

    errors: list[tuple[str, int, str]] = []
    for path in source_files:
        resolved = Path(path).resolve()
        text = variable_texts.get(resolved)
        if text is None:
            text = _read_source(path)
        available = shared | local.get(resolved, set())
        for m in _VAR_USE_RE.finditer(text):
            var = m.group(1)
            if var not in available:
                errors.append((path, text[: m.start()].count("\n") + 1, var))
    return errors


def extract_css_classes(text: str) -> set[str]:
    cleaned = _CSS_GLOBAL_RE.sub("", _strip_comments(text))
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

    ts_texts = {path: _read_source(path) for path in ts_files}

    errors: list[str] = []
    for css_path in module_css:
        css_text = _read_source(css_path)

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


def _blank_span(match: re.Match[str]) -> str:
    return re.sub(r"[^\n]", " ", match.group(0))


def check_hardcoded_colors(files: list[str], *, check_named_colors: bool = True) -> list[tuple[str, int, str]]:
    """Report raw colors outside CSS custom-property declarations.

    Functional colors that take their color from a var() (for example,
    rgb(var(--color-shadow-rgb), 0.5)) are token-based and stay allowed.
    Named colors are checked in CSS, but not TypeScript, where words such as
    "red" and "green" are also component variant names and visible copy.
    """
    errors: list[tuple[str, int, str]] = []
    seen: set[tuple[str, int, str]] = set()
    for path in files:
        text = _PROP_DEF_RE.sub(_blank_span, _read_source(path))
        for lineno, line in enumerate(text.splitlines(), start=1):
            values: list[str] = []
            for m in _HEX_COLOR_RE.finditer(line):
                values.append(m.group(0).lower())
            for m in _FUNC_COLOR_RE.finditer(line):
                if "var(" in m.group(2):
                    continue
                values.append(f"{m.group(1).lower()}({re.sub(r'\s+', '', m.group(2))})")
            if check_named_colors:
                values.extend(m.group(1).lower() for m in _NAMED_COLOR_RE.finditer(line))
            for value in values:
                key = (path, lineno, value)
                if key not in seen:
                    seen.add(key)
                    errors.append(key)
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--source-path",
        type=Path,
        default=Path("frontend/src"),
        help="frontend source directory (default: %(default)s)",
    )
    parser.add_argument(
        "--token-file",
        type=Path,
        default=Path("frontend/src/global.css"),
        help="shared CSS token file (default: %(default)s)",
    )
    args = parser.parse_args()

    try:
        files = subprocess.check_output(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard", str(args.source_path)],
            text=True,
        ).splitlines()
        files.sort()
    except subprocess.CalledProcessError as e:
        print(f"Error running git: {e}", file=sys.stderr)
        return 1

    css_files = [f for f in files if f.endswith(".css")]
    html_files = [f for f in files if f.endswith(".html")]
    variable_files = css_files + html_files
    token_file = str(args.token_file)
    if token_file not in variable_files:
        variable_files.append(token_file)
    source_files = [f for f in files if f.endswith((".css", ".html", ".ts", ".tsx"))]

    raw_color_source_files = [f for f in source_files if f.endswith((".ts", ".tsx"))]
    try:
        var_errors = check_css_vars(variable_files, source_files, token_file)
        selector_errors = check_unused_selectors(files)
        color_errors = check_hardcoded_colors(css_files + html_files)
        color_errors += check_hardcoded_colors(raw_color_source_files, check_named_colors=False)
    except (OSError, UnicodeError) as e:
        print(f"Error reading source file: {e}", file=sys.stderr)
        return 1

    rc = 0
    if var_errors:
        print("Error: undefined CSS custom properties:")
        for path, line, var in sorted(var_errors):
            print(f"  {path}:{line}: {var}")
        rc = 1

    if selector_errors:
        print("Error: CSS module selector issues:")
        for msg in selector_errors:
            print(msg)
        rc = 1

    if color_errors:
        print(f"Error: hardcoded color values (use var(); shared tokens: {args.token_file}):")
        for path, line, value in sorted(color_errors):
            print(f"  {path}:{line}: {value}")
        rc = 1

    return rc


if __name__ == "__main__":
    sys.exit(main())
