#!/usr/bin/env python3
"""Update AGENTS.md with a file index based on first-line comments."""

import os
import re
import subprocess
import sys

# One entry per workspace AGENTS.md to update.
# "exclude_dirs" lists non-workspace directories to skip (e.g. generated files).
# Sub-workspace exclusions and AGENTS.md inclusion are computed automatically.
CONFIGS = [
    {"root_dir": ".github"},
    {"root_dir": "android"},
    {"root_dir": "backend"},
    {"root_dir": "frontend"},
    {"root_dir": "."},
]


def get_git_files():
    try:
        result = subprocess.run(["git", "ls-files", "-z"], capture_output=True, text=True, check=True)
        return [f for f in result.stdout.split("\0") if f]
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        print(f"Error listing git files: {e}", file=sys.stderr)
        return []


def get_file_comment(filepath):
    # Extensions to check and their comment prefixes
    extensions = {
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
    }

    fname = os.path.basename(filepath)
    if fname in ("AGENTS.md", "CLAUDE.md"):
        return None

    # Skip generated CSS type declarations (*.module.css.d.ts, *.css.d.ts)
    if filepath.endswith(".css.d.ts"):
        return None
    _, ext = os.path.splitext(filepath)
    prefix = extensions.get(ext)
    if not prefix:
        if fname == "Makefile":
            prefix = "#"
        elif "Dockerfile" in fname:
            prefix = "#"
        else:
            return None

    try:
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
                if not comment:
                    continue
                return comment

            # Hit code before a comment
            return None
    except Exception:
        return None
    return None


def resolve_configs(configs):
    """Derive target_file and sub-workspace exclusions for each config."""
    resolved = []
    for cfg in configs:
        root = cfg["root_dir"]
        target = "AGENTS.md" if root == "." else f"{root}/AGENTS.md"
        exclude = set(cfg.get("exclude_dirs", set()))
        resolved.append({"root_dir": root, "target_file": target, "exclude_dirs": exclude})

    # For each config, find child workspaces (other configs whose root is a
    # direct subdirectory) and add them to exclude_dirs.
    for cfg in resolved:
        root = cfg["root_dir"]
        prefix = "" if root == "." else root + "/"
        for other in resolved:
            oroot = other["root_dir"]
            if oroot == root:
                continue
            if prefix == "":
                child_rel = oroot
            elif oroot.startswith(prefix):
                child_rel = oroot[len(prefix) :]
            else:
                continue
            if "/" not in child_rel:
                cfg["exclude_dirs"].add(child_rel)
    return resolved


def generate_index(config, all_files, sub_workspace_targets):
    root_dir = config["root_dir"]
    exclude = config["exclude_dirs"]
    target = config["target_file"]
    files_found = []

    for filepath in all_files:
        # Skip own AGENTS.md
        if filepath == target:
            continue
        # Scope to root_dir
        if root_dir == ".":
            relpath = filepath
        else:
            if not filepath.startswith(root_dir + "/"):
                continue
            relpath = filepath[len(root_dir) + 1 :]

        # Check excluded subdirectories, but let sub-workspace AGENTS.md through
        rel_parts = relpath.replace("\\", "/").split("/")
        if rel_parts[0] in exclude:
            if filepath not in sub_workspace_targets:
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


def main():
    all_files = get_git_files()
    if not all_files:
        print("No files found in git repository.")
        return 1
    resolved = resolve_configs(CONFIGS)
    all_targets = {cfg["target_file"] for cfg in resolved}
    for config in resolved:
        content = generate_index(config, all_files, all_targets)
        update_markdown(config["target_file"], content)
    return 0


if __name__ == "__main__":
    sys.exit(main())
