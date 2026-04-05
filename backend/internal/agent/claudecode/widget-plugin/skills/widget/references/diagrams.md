# Diagram Guidelines

## Decision Framework

Choose the diagram type based on the user's intent:

| User verb | Diagram type | Best for |
|-----------|-------------|----------|
| "explain", "how does" | Flowchart | Processes, algorithms, decision trees |
| "compare", "relationship" | Structural | Architecture, hierarchies, ER diagrams |
| "illustrate", "show" | Illustrative | Concepts, metaphors, artistic diagrams |
| "plan", "timeline" | Timeline / Gantt | Project phases, sequences |
| "break down" | Tree / hierarchy | Decomposition, org charts |

## Complexity Budget

Keep diagrams readable. Limits per type:

| Type | Max nodes | Max connectors | Max depth |
|------|-----------|---------------|-----------|
| Flowchart | 15 | 20 | 6 |
| Structural | 20 | 25 | 4 |
| Illustrative | 12 | 15 | 3 |
| Timeline | 10 phases | — | 2 |
| Tree | 20 | 19 | 5 |

If content exceeds these limits, split into multiple diagrams or add
a "detail" toggle (show/hide secondary nodes via script).

## Flowcharts

### Layout

- Flow direction: top-to-bottom (default) or left-to-right (for wide content).
- Node spacing: 80px vertical, 120px horizontal minimum.
- Start and end nodes: rounded rectangles with a distinct fill.
- Decision nodes: diamond shape (rotated square).

### Node Shapes

```
┌──────────┐   Start/end: rounded rect, primary fill
│  Start   │   rx=20, fill=var(--primary-bg)
└──────────┘

┌──────────┐   Process: standard rect
│ Process  │   rx=6, fill=var(--surface)
└──────────┘

    ◇         Decision: diamond (rotated rect)
   / \        fill=var(--accent-bg), stroke=var(--accent)
  / ? \
  \   /
   \ /
    ◇
```

### Connector Rules

- Use orthogonal paths (H/V segments) for flowcharts — not curved paths.
- Label connectors at the midpoint ("Yes" / "No" for decisions).
- Arrow direction indicates flow — always use `marker-end`.
- Merge paths converge to a single node — don't duplicate downstream nodes.

## Structural Diagrams

### Layout

- Use a grid or hierarchical layout (parent-child).
- Group related components in dashed boundary boxes:
  ```html
  <rect class="boundary" x="..." y="..." width="..." height="..."
        fill="none" stroke="var(--border)" stroke-dasharray="4,4" rx="8"/>
  ```
- Label boundaries with a title positioned at the top-left inside.

### Conventions

- Thick borders (2px) for primary components.
- Thin borders (1px) for secondary/internal components.
- Dashed connectors for optional/async relationships.
- Solid connectors for synchronous dependencies.
- Color-code by layer: e.g., blue for frontend, green for backend, amber for data.

## Illustrative Diagrams

### Style

- Use color fills and larger shapes for visual impact.
- Icons can be approximated with emoji or simple SVG paths.
- Less rigid layout — organic positioning is acceptable.
- Use opacity and scale to create depth (primary = 100%, secondary = 70%).

### Annotations

- Use callout lines (thin, dashed) connecting labels to elements.
- Position labels outside the main diagram area.
- Use a consistent font size (12px) for all annotations.

## General Tips

1. **Title.** Add a title text element at the top of the SVG (18px, bold).
2. **Legend.** If using color-coding, add a small legend in the bottom-right.
3. **Whitespace.** Leave ≥20px margin inside the viewBox boundary.
4. **Alignment.** Align node centers to a grid. Avoid pixel-level tweaking.
5. **Animation.** Avoid animations in diagrams — they distract from the content.
   If interactivity is needed, use hover highlights via CSS.
