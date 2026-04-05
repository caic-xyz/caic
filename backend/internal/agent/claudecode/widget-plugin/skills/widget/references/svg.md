# SVG Guidelines

## ViewBox Setup

Always use a `viewBox` attribute — never fixed `width`/`height` in pixels:

```html
<svg viewBox="0 0 800 500" style="width:100%; height:auto;">
```

### Safety Checklist

1. Set `viewBox` to the content bounding box.
2. Use `style="width:100%; height:auto"` for responsiveness.
3. Add 20–40px padding inside the viewBox to prevent clipping.
4. Test at 400px and 1200px wide viewports.

## Font Width Calibration

SVG text width is unpredictable. Use these character-width constants for
layout calculations:

| Font size | Avg char width | Suitable for |
|-----------|---------------|--------------|
| 14px | 8.4px | Body text, labels |
| 13px | 7.8px | Small labels |
| 12px | 7.2px | Annotations |
| 11px | 6.6px | Fine print |

Calculate label width: `text.length * avgCharWidth + 16px` (padding).

Use `text-anchor="middle"` for centered labels, `"start"` for left-aligned.

## CSS Classes for SVG

Define reusable classes in the `<style>` block:

```css
.node-rect {
  fill: var(--surface);
  stroke: var(--border);
  stroke-width: 1;
  rx: 6;
}
.node-text {
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
  font-size: 14px;
  fill: var(--text-primary);
  dominant-baseline: central;
}
.connector {
  fill: none;
  stroke: var(--border);
  stroke-width: 1.5;
}
.connector-label {
  font-size: 12px;
  fill: var(--text-secondary);
  text-anchor: middle;
}
.highlight-rect {
  fill: var(--primary-bg);
  stroke: var(--primary);
  stroke-width: 1.5;
  rx: 6;
}
```

## Arrow Markers

Define arrow markers in a `<defs>` block at the top of the SVG:

```html
<defs>
  <marker id="arrow" viewBox="0 0 10 7" refX="10" refY="3.5"
          markerWidth="10" markerHeight="7" orient="auto-start-reverse">
    <polygon points="0 0, 10 3.5, 0 7" fill="var(--border)"/>
  </marker>
  <marker id="arrow-highlight" viewBox="0 0 10 7" refX="10" refY="3.5"
          markerWidth="10" markerHeight="7" orient="auto-start-reverse">
    <polygon points="0 0, 10 3.5, 0 7" fill="var(--primary)"/>
  </marker>
</defs>
```

Use with: `marker-end="url(#arrow)"` on `<line>` or `<path>` elements.

## Connectors

- **Straight lines:** Use `<line>` for simple connections.
- **Curved paths:** Use `<path>` with cubic Bézier for organic flow:
  ```html
  <path d="M100,50 C150,50 150,150 200,150" class="connector" marker-end="url(#arrow)"/>
  ```
- **Orthogonal paths:** Use `<path>` with horizontal/vertical segments:
  ```html
  <path d="M100,50 H150 V150 H200" class="connector" marker-end="url(#arrow)"/>
  ```

## Fill Rules

- Node backgrounds: use `var(--surface)` or a color ramp stop 50.
- Highlighted nodes: use `var(--primary-bg)` with `var(--primary)` stroke.
- Connector lines: use `var(--border)` for default, `var(--primary)` for emphasis.
- Text: use `fill` not `color` — SVG text uses `fill`.

## Grouping

Use `<g>` elements to group related nodes and connectors. Apply transforms
to groups for positioning:

```html
<g transform="translate(100, 50)">
  <rect class="node-rect" width="160" height="40"/>
  <text class="node-text" x="80" y="20" text-anchor="middle">Label</text>
</g>
```

## Interactivity in SVG

- Add `cursor: pointer` and `:hover` styles to clickable groups.
- Use `opacity` transitions for hover effects (not transforms — they
  can cause layout jank in SVG).
- Event listeners should be added in the `<script>` block, not inline
  `onclick` attributes.
