#!/usr/bin/env python3
"""Update AGENTS.md files (containing a file index marker) with an auto-generated index.

To opt-in a directory, add these two markers to its AGENTS.md:

    <!-- BEGIN FILE INDEX -->
    <!-- END FILE INDEX -->

The script auto-discovers all AGENTS.md files tracked by git that contain the
markers, generates a file index from first-line comments, and injects it between
the markers. It also ensures a CLAUDE.md symlink exists next to every AGENTS.md.
"""

import fnmatch
import os
import re
import subprocess
import sys


def get_git_files():
    try:
        result = subprocess.run(["git", "ls-files", "-z"], capture_output=True, text=True, check=True)
        return [f for f in result.stdout.split("\0") if f]
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        print(f"Error listing git files: {e}", file=sys.stderr)
        return []


def get_file_comment(filepath):
    # Glob patterns mapping filenames to their comment prefix. None skips the file.
    comment_prefixes = {
        "*.css.d.ts": None,
        "*.cjs": "//",
        "*.go": "//",
        "*.js": "//",
        "*.kt": "//",
        "*.md": "#",
        "*.mjs": "//",
        "*.py": "#",
        "*.sh": "#",
        "*.swift": "//",
        "*.ts": "//",
        "*.tsx": "//",
        "*.yaml": "#",
        "*.yml": "#",
        "Dockerfile*": "#",
        "Makefile": "#",
    }
    if os.path.islink(filepath):
        return None
    fname = os.path.basename(filepath)
    prefix = next((p for pat, p in comment_prefixes.items() if fnmatch.fnmatch(fname, pat)), None)
    if not prefix:
        return None
    with open(filepath, "r", encoding="utf-8") as f:
        lines = [f.readline() for _ in range(10)]
    start_idx = 1 if (lines[0] and lines[0].startswith("#!")) else 0
    for i in range(start_idx, len(lines)):
        line = lines[i]
        if not line:
            break
        sline = line.strip()
        if not sline:
            continue
        # Python docstring: extract first line of a triple-quoted string
        if fname.endswith(".py") and (sline.startswith('"""') or sline.startswith("'''")):
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
            if not comment:
                continue
            return comment
        # Hit code before a comment
        return None
    return None


def discover_configs(all_files):
    """Auto-discover workspace roots from AGENTS.md files that contain a file index marker.

    Returns a dict mapping target AGENTS.md path to its set of excluded child directories.
    """
    candidates = sorted(f for f in all_files if os.path.basename(f) == "AGENTS.md")
    configs = {}
    for f in candidates:
        with open(f, "r", encoding="utf-8") as fh:
            if "<!-- BEGIN FILE INDEX -->" in fh.read():
                configs[f] = set()
    # For each config, find child workspaces and add them to exclude_dirs.
    for target, exclude in configs.items():
        root = os.path.dirname(target)
        prefix = root + "/" if root else ""
        for other_target in configs:
            oroot = os.path.dirname(other_target)
            if oroot == root:
                continue
            if not prefix:
                child_rel = oroot
            elif oroot.startswith(prefix):
                child_rel = oroot[len(prefix) :]
            else:
                continue
            if "/" not in child_rel:
                exclude.add(child_rel)
    return configs


def generate_index(target, exclude, all_files, all_configs):
    root_dir = os.path.dirname(target)
    files_found = []
    for filepath in all_files:
        # Skip own AGENTS.md
        if filepath == target:
            continue
        # Scope to root_dir
        if root_dir:
            if not filepath.startswith(root_dir + "/"):
                continue
            relpath = filepath[len(root_dir) + 1 :]
        else:
            relpath = filepath

        # Check excluded subdirectories, but let sub-workspace AGENTS.md through
        rel_parts = relpath.replace("\\", "/").split("/")
        if rel_parts[0] in exclude:
            if filepath not in all_configs:
                continue
        comment = get_file_comment(filepath)
        if comment:
            files_found.append((relpath, comment))
    desc = "Autogenerated from first-line comments. Run scripts/update_agents_file_index.py to refresh."
    lines = ["## File Index", "", desc, ""]
    for path, comment in sorted(files_found):
        lines.append(f"- `{path}`: {comment}")
    return "\n".join(lines)


def update_markdown(target_file, content):
    if not os.path.exists(target_file):
        print(f"Warning: {target_file} not found, skipping.")
        return
    start = "<!-- BEGIN FILE INDEX -->"
    end = "<!-- END FILE INDEX -->"
    with open(target_file, "r", encoding="utf-8") as f:
        original = f.read()
    new_section = f"{start}\n{content}\n{end}"
    if start in original and end in original:
        pattern = re.compile(f"{re.escape(start)}.*?{re.escape(end)}", re.DOTALL)
        updated = pattern.sub(new_section, original)
    else:
        updated = (original.rstrip() + "\n\n" + new_section + "\n") if original.strip() else (new_section + "\n")
    if updated == original:
        return
    with open(target_file, "w", encoding="utf-8") as f:
        f.write(updated)
    print(f"Updated: {target_file}")


def ensure_claude_symlinks(all_files):
    """Ensure every AGENTS.md has a sibling CLAUDE.md symlink pointing to it."""
    for f in all_files:
        if os.path.basename(f) != "AGENTS.md":
            continue
        d = os.path.dirname(f) or "."
        link = os.path.join(d, "CLAUDE.md")
        if os.path.islink(link) and os.readlink(link) == "AGENTS.md":
            continue
        if os.path.exists(link):
            print(f"Error: {link} exists but is not a symlink to AGENTS.md.", file=sys.stderr)
            return 1
        os.symlink("AGENTS.md", link)
        print(f"Created: {link} -> AGENTS.md")
    return 0


def main():
    all_files = get_git_files()
    if not all_files:
        print("No files found in git repository.")
        return 1
    ret = ensure_claude_symlinks(all_files)
    if ret:
        return ret
    configs = discover_configs(all_files)
    for target, exclude in configs.items():
        update_markdown(target, generate_index(target, exclude, all_files, configs))
    return 0


if __name__ == "__main__":
    sys.exit(main())
