#!/usr/bin/env python3
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

"""Update generated backend architecture dependency diagrams."""

import argparse
import json
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DOC_PATH = ROOT / "backend" / "docs" / "ARCHITECTURE.md"
START_MARKER = "<!-- BEGIN GENERATED PACKAGE DEPENDENCIES -->"
END_MARKER = "<!-- END GENERATED PACKAGE DEPENDENCIES -->"


@dataclass(frozen=True)
class Package:
    import_path: str
    path: str
    imports: tuple[str, ...]


@dataclass(frozen=True)
class GroupRule:
    name: str
    exact: tuple[str, ...] = ()
    prefixes: tuple[str, ...] = ()


GROUP_RULES = (
    GroupRule("Commands", prefixes=("cmd/", "internal/cmd/")),
    GroupRule("Static Assets", exact=("frontend",)),
    GroupRule("Agent", exact=("internal/agent",), prefixes=("internal/agent/",)),
    GroupRule("Forge", exact=("internal/forge",), prefixes=("internal/forge/",)),
    GroupRule("Server", exact=("internal/server",), prefixes=("internal/server/",)),
    GroupRule(
        "Task Runtime",
        exact=("internal/container", "internal/task", "internal/tasks"),
        prefixes=("internal/container/", "internal/task/", "internal/tasks/"),
    ),
    GroupRule("Support", prefixes=("internal/",)),
)
AGENT_GROUP_RULE = next(rule for rule in GROUP_RULES if rule.name == "Agent")


def run(args: list[str]) -> str:
    result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True, check=True)
    return result.stdout


def read_json_stream(raw: str) -> list[dict[str, object]]:
    decoder = json.JSONDecoder()
    pos = 0
    values = []
    while pos < len(raw):
        match = re.search(r"\S", raw[pos:])
        if not match:
            break
        pos += match.start()
        value, pos = decoder.raw_decode(raw, pos)
        if not isinstance(value, dict):
            raise ValueError("go list -json returned a non-object JSON value")
        values.append(value)
    return values


def backend_packages() -> list[Package]:
    module_path = run(["go", "list", "-m"]).strip()
    backend_prefix = f"{module_path}/backend/"
    raw = run(["go", "list", "-json", "./backend/..."])
    packages = []
    for value in read_json_stream(raw):
        import_path = str(value["ImportPath"])
        if not import_path.startswith(backend_prefix):
            continue
        imports = tuple(
            sorted(
                dep.removeprefix(backend_prefix)
                for dep in value.get("Imports", [])
                if isinstance(dep, str) and dep.startswith(backend_prefix)
            )
        )
        packages.append(
            Package(
                import_path=import_path,
                path=import_path.removeprefix(backend_prefix),
                imports=imports,
            )
        )
    return sorted(packages, key=lambda package: package.path)


def node_id(package_path: str) -> str:
    return "pkg_" + re.sub(r"[^a-zA-Z0-9_]", "_", package_path)


def matches_rule(package_path: str, rule: GroupRule) -> bool:
    return package_path in rule.exact or package_path.startswith(rule.prefixes)


def dependency_closure(packages_by_path: dict[str, Package], roots: set[str]) -> set[str]:
    seen = set()
    pending = list(sorted(roots & packages_by_path.keys()))
    while pending:
        path = pending.pop()
        if path in seen:
            continue
        seen.add(path)
        pending.extend(dep for dep in packages_by_path[path].imports if dep not in seen)
    return seen


def grouped_paths(package_paths: set[str]) -> list[tuple[str, list[str]]]:
    remaining = set(package_paths)
    groups = []
    for rule in GROUP_RULES:
        paths = sorted(path for path in remaining if matches_rule(path, rule))
        if not paths:
            continue
        groups.append((rule.name, paths))
        remaining.difference_update(paths)
    if remaining:
        groups.append(("Other", sorted(remaining)))
    return groups


def edge_lines(packages: list[Package], included: set[str]) -> list[str]:
    lines = []
    for package in packages:
        if package.path not in included:
            continue
        for dep in package.imports:
            if dep in included:
                lines.append(f"  {node_id(package.path)} --> {node_id(dep)}")
    return lines


def render_nodes(package_paths: list[str]) -> list[str]:
    return [f'  {node_id(path)}["{path}"]' for path in package_paths]


def render_graph(
    title: str,
    packages: list[Package],
    included: set[str],
    grouped: bool = False,
) -> list[str]:
    existing = {package.path for package in packages}
    included = included & existing
    lines = [f"## {title}", "", "```mermaid", "graph TD"]
    if grouped:
        assigned_paths = set()
        for group_name, visible_paths in grouped_paths(included):
            group_id = node_id(group_name).removeprefix("pkg_")
            lines.append(f'  subgraph {group_id}["{group_name}"]')
            lines.extend("  " + line for line in render_nodes(visible_paths))
            lines.append("  end")
            lines.append("")
            assigned_paths.update(visible_paths)
        remaining_paths = sorted(included - assigned_paths)
        if remaining_paths:
            lines.extend(render_nodes(remaining_paths))
            lines.append("")
    else:
        lines.extend(render_nodes(sorted(included)))
        lines.append("")
    lines.extend(edge_lines(packages, included))
    lines.extend(["```", ""])
    return lines


def render_table(packages: list[Package]) -> list[str]:
    lines = [
        "## Package Dependencies",
        "",
        "| Package | Direct backend dependencies |",
        "|---|---|",
    ]
    for package in packages:
        deps = ", ".join(f"`{dep}`" for dep in package.imports) if package.imports else "None"
        lines.append(f"| `{package.path}` | {deps} |")
    lines.append("")
    return lines


def render_generated_section(packages: list[Package]) -> str:
    packages_by_path = {package.path: package for package in packages}
    all_paths = {package.path for package in packages}
    runtime_spine_paths = dependency_closure(packages_by_path, {"cmd/caic"})
    agent_paths = dependency_closure(
        packages_by_path,
        {path for path in all_paths if matches_rule(path, AGENT_GROUP_RULE)},
    )
    lines = [
        "## Generated Package Dependencies",
        "",
        "This section is generated by `scripts/update_backend_architecture.py`.",
        "It uses direct package imports from `go list -json ./backend/...`.",
        "",
        "Arrows point from the importing package to the package it imports. External",
        "dependencies are omitted.",
        "",
    ]
    lines.extend(render_graph("Runtime Spine", packages, runtime_spine_paths))
    lines.extend(render_graph("Agent Backends", packages, agent_paths))
    lines.extend(render_graph("Complete Package Import Graph", packages, all_paths, grouped=True))
    lines.extend(render_table(packages))
    return "\n".join(lines).rstrip()


def update_markdown(content: str, check: bool) -> int:
    original = DOC_PATH.read_text(encoding="utf-8")
    replacement = f"{START_MARKER}\n{content}\n{END_MARKER}"
    pattern = re.compile(f"{re.escape(START_MARKER)}.*?{re.escape(END_MARKER)}", re.DOTALL)
    if not pattern.search(original):
        raise ValueError(f"{DOC_PATH} is missing generated section markers")
    updated = pattern.sub(replacement, original)
    if updated == original:
        return 0
    if check:
        print(
            f"Error: {DOC_PATH.relative_to(ROOT)} generated section is out of date. "
            "Run scripts/update_backend_architecture.py to fix.",
            file=sys.stderr,
        )
        return 1
    DOC_PATH.write_text(updated, encoding="utf-8")
    print(f"Updated: {DOC_PATH.relative_to(ROOT)}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="check that the generated section is up to date without modifying files",
    )
    args = parser.parse_args()
    return update_markdown(render_generated_section(backend_packages()), args.check)


if __name__ == "__main__":
    sys.exit(main())
