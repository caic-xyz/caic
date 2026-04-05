# Core Design System

## Philosophy

- **Streaming-first.** Structure code so useful content appears progressively.
  Place `<style>` first (keep it short), then content HTML, then `<script>`
  last. Scripts execute only after streaming completes.
- **Flat design.** No gradients, shadows, `box-shadow`, or blur. Use solid
  borders (1px) for separation.
- **Transparent background.** The host app provides the background. Never set
  `background` on the root element.
- **Responsive.** Use `width: 100%` and relative units. The widget container
  may be narrow (mobile) or wide (desktop).

## Typography

Use the system font stack everywhere:

```css
font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
```

| Role | Size | Weight |
|------|------|--------|
| Title / heading | 18px | 600 |
| Subheading | 14px | 600 |
| Body text | 14px | 400 |
| Caption / label | 12px | 500 |
| Monospace code | 13px | 400 |

Monospace stack: `'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace`.

Line height: 1.5 for body text, 1.3 for headings.

## Color Variables

Define colors as CSS custom properties inside a `<style>` block. Always provide
both light and dark mode values using `prefers-color-scheme`:

```css
<style>
  :root {
    --text-primary: #1a1a2e;
    --text-secondary: #555;
    --border: #e0e0e0;
    --surface: rgba(0,0,0,0.03);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --text-primary: #e0e0e0;
      --text-secondary: #999;
      --border: #333;
      --surface: rgba(255,255,255,0.05);
    }
  }
</style>
```

Always use these variables — never hardcode hex colors in content HTML.

## Dark Mode

Every widget **must** support dark mode via `prefers-color-scheme: dark`.
Define all colors as CSS custom properties and override them in the dark
media query. Test that text remains readable against the transparent
background in both modes.

## CDN Allowlist

Scripts may load libraries only from these CDNs:

- `https://cdnjs.cloudflare.com/ajax/libs/...`
- `https://cdn.jsdelivr.net/npm/...`
- `https://unpkg.com/...`
- `https://esm.sh/...`

Common libraries and their CDN URLs:

| Library | URL |
|---------|-----|
| Chart.js 4 | `https://cdn.jsdelivr.net/npm/chart.js@4` |
| D3.js 7 | `https://cdn.jsdelivr.net/npm/d3@7` |
| Three.js | `https://cdn.jsdelivr.net/npm/three@latest` |
| KaTeX | `https://cdn.jsdelivr.net/npm/katex@latest/dist/katex.min.js` |
| Mermaid | `https://cdn.jsdelivr.net/npm/mermaid@latest/dist/mermaid.min.js` |
| Tone.js | `https://cdn.jsdelivr.net/npm/tone@latest` |

**No other external sources.** No `fetch()`, `XMLHttpRequest`, or WebSocket.
All data must be inline.

## Spacing & Layout

- Use `padding: 16px` as default container padding.
- Use `gap` with flexbox/grid — never margin hacks.
- Standard gap values: 8px (tight), 12px (default), 16px (spacious), 24px (section).
- Prefer `flexbox` for single-axis layouts, `grid` for 2D layouts.
- `border-radius: 8px` for cards, `4px` for small elements (badges, inputs).

## Interactivity

- Buttons and interactive elements need `:hover` and `:active` states.
- Use `cursor: pointer` on clickable elements.
- Transitions: `transition: all 0.15s ease` for hover effects.
- Focus states: `outline: 2px solid var(--focus-ring)` for accessibility.

## Structure Rules

1. No `<!DOCTYPE>`, `<html>`, `<head>`, or `<body>` tags.
2. Output a raw HTML fragment.
3. Each widget is self-contained — no references to other widgets.
4. Maximum one `<style>` block, placed first.
5. Maximum one `<script>` block, placed last.
6. All data must be inline (no fetch/XHR/WebSocket).
