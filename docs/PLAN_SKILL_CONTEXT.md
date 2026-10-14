# Task detail shows the skills in the agent's current context

Task detail lists the skills the main agent has loaded since its context last
started (task start, compaction, or clear). Each skill links to its `SKILL.md`
in the markdown viewer. After a compaction or clear the list is empty until the
agent loads a skill again; earlier context sections keep their own lists.
`agent.SkillLoadMessage` carries confirmed main-context loads;
`agent.SkillReadMessage` stays analytics only.

## Phase 1 — skill-load-paths: Links open the file the agent read

- **Scope:** Path extraction for inferred `agent.SkillLoadMessage` paths. File tools (`Read`,
  `read`, OpenCode `file_path`) keep the literal path, spaces included. Shell
  operands resolve a relative path against the command's recorded working
  directory (Codex `cwd`); a path that cannot be resolved reliably keeps the
  skill name and omits the path.
- **Preserve:** Skill-name inference and the shell-segment rules in
  `agent/skillread.go`; analytics counts.
- **Verify:** Go tests for a spaced absolute path, a relative Codex read with a
  `cwd`, and a `cd`-dependent command that omits the path.
  `BenchmarkSkillsFromCommand` delta reported.

## Phase 2 — context-skills-ux: Skills scoped to context sections

- **Scope:** Frontend, consuming the `skillLoad` SSE event. One fold owns
  loaded skills per context section; a
  section ends at a `compact_boundary` or `context_cleared` system event. Within
  a section, entries are unique by path, else by name; a path-less entry takes
  the path of a later load of the same name only when one path is known. The
  task header shows the current section's skills and hides the strip when it is
  empty. Each earlier section's boundary in the transcript shows that section's
  skills with the same links. The fake e2e agent can emit skill loads and a
  compaction so a browser test can cover the journey.
- **Preserve:** The `menu_book` icon and markdown-viewer links; recursive
  document-relative navigation in the viewer.
- **Verify:** Unit tests for the fold (reset at both boundaries, same-name
  different paths, path enrichment). Playwright: loads, link opens the viewer,
  a compaction empties the header strip and the earlier section still lists
  its skills, at 390 px and 1280 px.

## Later

- Show skill annotations on `Read`/`Bash` tool cards and an unhidden `Skill` card.
- List a task's loaded skills on the TaskInfo page with path provenance.
