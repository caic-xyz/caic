---
name: widget
description: >
  Render interactive HTML widgets inline in the conversation. Use when the user
  asks for visualizations, diagrams, calculators, interactive demos, charts,
  data displays, or any rich UI that benefits from HTML rendering rather than
  plain text or markdown.
allowed-tools:
  - "mcp__plugin_caic-widget_widget__show_widget"
---

# Widget Rendering

You MUST call the `mcp__plugin_caic-widget_widget__show_widget` tool to render
widgets. Do NOT output HTML as text — it will not render correctly. Always pass
the HTML through the tool's `widget_code` parameter.

The tool response will say "Widget rendered." — this is expected. The widget is
displayed to the user in a sandboxed iframe. Do NOT re-render or apologize.

## Before your first widget in a conversation

Read the design reference files in this skill's `references/` directory to
understand the design system. Load only what you need:

- **Always read**: `core.md` (philosophy, streaming rules, typography, CDN list)
- **For diagrams/flowcharts**: also read `svg.md` and `diagrams.md`
- **For UI mockups/forms**: also read `components.md` and `colors.md`
- **For data charts**: also read `charts.md` and `colors.md`
- **For artistic/illustrative**: also read `svg.md` and `colors.md`

## Quick rules

1. **No document structure tags.** No `<!DOCTYPE>`, `<html>`, `<head>`, `<body>`.
   Output a raw HTML fragment.
2. **Order: style → content → script.** Useful content must appear early for
   streaming. Scripts execute only after streaming completes.
3. **CDN allowlist.** Scripts may only load from: `cdnjs.cloudflare.com`,
   `cdn.jsdelivr.net`, `unpkg.com`, `esm.sh`. No other external sources.
4. **No network requests.** `fetch()`, `XMLHttpRequest`, WebSocket are blocked.
   All data must be inline.
5. **Self-contained.** Each widget is a standalone fragment. No imports from
   previous widgets.
6. **Title format.** Use `snake_case` for the title parameter.
