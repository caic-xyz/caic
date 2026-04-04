#!/usr/bin/env python3
"""Update AGENTS.md with a file index based on first-line comments."""

import os
import re
import subprocess
import sys

# Common Configuration
SECTION_START = "<!-- BEGIN FILE INDEX -->"
SECTION_END = "<!-- END FILE INDEX -->"
SECTION_TITLE = "## File Index"

# Extensions to check and their comment prefixes
EXTENSIONS = {
    ".cjs": "//",
    ".go": "//",
    ".js": "//",
    ".kt": "//",
    ".md": "#",
    ".mjs": "//",
    ".py": "#",
    ".sh": "#",
    ".swift": "//",
    ".ts": "//",
    ".tsx": "//",
    ".yaml": "#",
    ".yml": "#",
    "Makefile": "#",
}

IGNORE_FILES = {
    "AGENTS.md",
    "CLAUDE.md",
}

# Configuration for each scan
CONFIGS = [
    {
        "root_dir": ".github",
        "target_file": ".github/AGENTS.md",
        "exclude_dirs": set(),
    },
    {
        "root_dir": "android",
        "target_file": "android/AGENTS.md",
        "exclude_dirs": set(),
    },
    {
        "root_dir": "backend",
        "target_file": "backend/AGENTS.md",
        "exclude_dirs": set(),
    },
    {
        "root_dir": "frontend",
        "target_file": "frontend/AGENTS.md",
        "exclude_dirs": set(),
    },
    {
        "root_dir": ".",
        "target_file": "AGENTS.md",
        "exclude_dirs": {".github", "android", "backend", "frontend"},
    },
]


def get_git_files():
    try:
        # Use -z to handle spaces in filenames correctly, though rare
        result = subprocess.run(["git", "ls-files", "-z"], capture_output=True, text=True, check=True)
        # Split by null terminator and filter empty strings
        return [f for f in result.stdout.split("\0") if f]
    except subprocess.CalledProcessError as e:
        print(f"Error listing git files: {e}", file=sys.stderr)
        return []
    except FileNotFoundError:
        print("Error: git not found", file=sys.stderr)
        return []


def get_file_comment(filepath):
    try:
        with open(filepath, "r", encoding="utf-8") as f:
            lines = [f.readline() for _ in range(10)]

            # Check extension/filename for comment style
            _, ext = os.path.splitext(filepath)
            fname = os.path.basename(filepath)
            prefix = EXTENSIONS.get(ext) or EXTENSIONS.get(fname)
            if not prefix:
                if fname == "Makefile":
                    prefix = "#"
                elif "Dockerfile" in fname:
                    prefix = "#"
                else:
                    return None

            # Skip shebang if present
            start_idx = 0
            if lines[0] and lines[0].startswith("#!"):
                start_idx = 1

            for i in range(start_idx, len(lines)):
                line = lines[i]
                if not line:
                    break
                sline = line.strip()
                if not sline:
                    continue

                # Python docstring: extract first line of a triple-quoted string
                if ext == ".py" and (sline.startswith('"""') or sline.startswith("'''")):
                    quote = sline[:3]
                    # Single-line docstring: """text"""
                    if sline.endswith(quote) and len(sline) > 6:
                        return sline[3:-3].strip()
                    # Multi-line docstring: return the first line
                    content = sline[3:].strip()
                    if content:
                        return content
                    # Opening quotes on their own line; use next non-empty line
                    for j in range(i + 1, len(lines)):
                        if lines[j] and lines[j].strip():
                            return lines[j].strip()
                    return None

                # Skip common directives/metadata that aren't descriptions
                if sline.startswith(f"{prefix}go:"):
                    continue
                if sline.startswith(f"{prefix} +build"):
                    continue
                if sline.startswith(f"{prefix} nolint"):
                    continue
                if sline.startswith(f"{prefix} swift-tools-version:"):
                    continue

                if sline.startswith(prefix):
                    comment = sline[len(prefix) :].strip()
                    # Skip empty comments
                    if not comment:
                        continue
                    return comment

                # If we hit code before a comment, give up
                return None
    except Exception:
        return None
    return None


def generate_index_for_config(config, all_files):
    root_dir = config["root_dir"]
    exclude = config["exclude_dirs"]
    files_found = []
    for filepath in all_files:
        # Filter based on root_dir
        if root_dir == ".":
            # For root scan, exclude files that start with excluded directories
            if any(filepath.startswith(ex + "/") or filepath == ex for ex in exclude):
                continue
            # relpath is just the filepath
            relpath = filepath
        else:
            # For subdir scan, only include files inside root_dir
            if not filepath.startswith(root_dir + "/"):
                continue
            # relpath is relative to root_dir
            relpath = filepath[len(root_dir) + 1 :]

        # Filter ignored files
        if os.path.basename(filepath) in IGNORE_FILES:
            continue

        # Check extension
        _, ext = os.path.splitext(filepath)
        fname = os.path.basename(filepath)
        if ext in EXTENSIONS or fname in EXTENSIONS:
            comment = get_file_comment(filepath)
            if comment:
                files_found.append((relpath, comment))

    files_found.sort()
    lines = []
    lines.append(SECTION_TITLE)
    lines.append("")
    lines.append("Autogenerated file index based on first-line comments.")
    lines.append("")
    for path, comment in files_found:
        lines.append(f"- `{path}`: {comment}")
    return "\n".join(lines)


def update_markdown_file(target_file, content):
    if not os.path.exists(target_file):
        print(f"Warning: {target_file} not found. Skipping.")
        return

    with open(target_file, "r", encoding="utf-8") as f:
        original = f.read()

    new_section = f"{SECTION_START}\n{content}\n{SECTION_END}"

    if SECTION_START in original and SECTION_END in original:
        # Replace existing
        pattern = re.compile(f"{re.escape(SECTION_START)}.*?{re.escape(SECTION_END)}", re.DOTALL)
        updated = pattern.sub(new_section, original)
    else:
        # Append
        if original.strip():
            updated = original.rstrip() + "\n\n" + new_section + "\n"
        else:
            updated = new_section + "\n"

    with open(target_file, "w", encoding="utf-8") as f:
        f.write(updated)


def main():
    all_files = get_git_files()
    if not all_files:
        print("No files found in git repository.")
        return 1
    for config in CONFIGS:
        content = generate_index_for_config(config, all_files)
        update_markdown_file(config["target_file"], content)
    return 0


if __name__ == "__main__":
    sys.exit(main())
