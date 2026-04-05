# Color Palette

Nine color ramps with 7 stops each. Use CSS custom properties to define colors
so dark mode overrides work correctly.

## Ramps

Each ramp has stops: `50` (lightest), `100`, `200`, `300`, `400`, `500`,
`600` (darkest). In dark mode, reverse the mapping (light stops for
backgrounds, dark stops for text).

### Purple
| Stop | Light | Dark |
|------|-------|------|
| 50 | #f3e8ff | #2d1b4e |
| 100 | #e9d5ff | #3b2667 |
| 200 | #d8b4fe | #7c3aed |
| 300 | #c084fc | #8b5cf6 |
| 400 | #a855f7 | #a78bfa |
| 500 | #7c3aed | #c4b5fd |
| 600 | #6d28d9 | #ddd6fe |

### Teal
| Stop | Light | Dark |
|------|-------|------|
| 50 | #f0fdfa | #0d2d2a |
| 100 | #ccfbf1 | #134e4a |
| 200 | #99f6e4 | #14b8a6 |
| 300 | #5eead4 | #2dd4bf |
| 400 | #2dd4bf | #5eead4 |
| 500 | #14b8a6 | #99f6e4 |
| 600 | #0d9488 | #ccfbf1 |

### Coral
| Stop | Light | Dark |
|------|-------|------|
| 50 | #fff1f0 | #3b1210 |
| 100 | #ffe4e1 | #5c1d1a |
| 200 | #ffc9c2 | #e8564a |
| 300 | #ffa69e | #f07068 |
| 400 | #f07068 | #ffa69e |
| 500 | #e8564a | #ffc9c2 |
| 600 | #c83c30 | #ffe4e1 |

### Blue
| Stop | Light | Dark |
|------|-------|------|
| 50 | #eff6ff | #172554 |
| 100 | #dbeafe | #1e3a6e |
| 200 | #bfdbfe | #3b82f6 |
| 300 | #93c5fd | #60a5fa |
| 400 | #60a5fa | #93c5fd |
| 500 | #3b82f6 | #bfdbfe |
| 600 | #2563eb | #dbeafe |

### Green
| Stop | Light | Dark |
|------|-------|------|
| 50 | #f0fdf4 | #14291a |
| 100 | #dcfce7 | #166534 |
| 200 | #bbf7d0 | #22c55e |
| 300 | #86efac | #4ade80 |
| 400 | #4ade80 | #86efac |
| 500 | #22c55e | #bbf7d0 |
| 600 | #16a34a | #dcfce7 |

### Amber
| Stop | Light | Dark |
|------|-------|------|
| 50 | #fffbeb | #2d1f04 |
| 100 | #fef3c7 | #78350f |
| 200 | #fde68a | #f59e0b |
| 300 | #fcd34d | #fbbf24 |
| 400 | #fbbf24 | #fcd34d |
| 500 | #f59e0b | #fde68a |
| 600 | #d97706 | #fef3c7 |

### Rose
| Stop | Light | Dark |
|------|-------|------|
| 50 | #fff1f2 | #2d1215 |
| 100 | #ffe4e6 | #881337 |
| 200 | #fecdd3 | #e11d48 |
| 300 | #fda4af | #fb7185 |
| 400 | #fb7185 | #fda4af |
| 500 | #e11d48 | #fecdd3 |
| 600 | #be123c | #ffe4e6 |

### Slate
| Stop | Light | Dark |
|------|-------|------|
| 50 | #f8fafc | #1e293b |
| 100 | #f1f5f9 | #334155 |
| 200 | #e2e8f0 | #64748b |
| 300 | #cbd5e1 | #94a3b8 |
| 400 | #94a3b8 | #cbd5e1 |
| 500 | #64748b | #e2e8f0 |
| 600 | #475569 | #f1f5f9 |

### Indigo
| Stop | Light | Dark |
|------|-------|------|
| 50 | #eef2ff | #1e1b4b |
| 100 | #e0e7ff | #2e2872 |
| 200 | #c7d2fe | #6366f1 |
| 300 | #a5b4fc | #818cf8 |
| 400 | #818cf8 | #a5b4fc |
| 500 | #6366f1 | #c7d2fe |
| 600 | #4f46e5 | #e0e7ff |

## Rules

1. **Max 3 ramps per widget.** Pick a primary ramp, an accent, and slate for
   neutral elements.
2. **WCAG AA contrast.** Text must have ≥4.5:1 contrast against its background.
   Use stop 600 text on stop 50 background (or reverse in dark mode).
3. **Semantic conventions:**
   - Green = success, positive, growth
   - Red/coral/rose = error, danger, decline
   - Amber = warning, caution
   - Blue = information, neutral highlight
   - Purple/indigo = primary action, brand
   - Teal = secondary action, alternative highlight
4. **Never use raw hex in HTML.** Define colors as CSS custom properties and
   reference them with `var(--color-name)`.

## Example

```css
<style>
  :root {
    --primary: #7c3aed;
    --primary-bg: #f3e8ff;
    --accent: #14b8a6;
    --accent-bg: #f0fdfa;
    --neutral: #64748b;
    --neutral-bg: #f8fafc;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --primary: #c4b5fd;
      --primary-bg: #2d1b4e;
      --accent: #5eead4;
      --accent-bg: #0d2d2a;
      --neutral: #cbd5e1;
      --neutral-bg: #1e293b;
    }
  }
</style>
```
