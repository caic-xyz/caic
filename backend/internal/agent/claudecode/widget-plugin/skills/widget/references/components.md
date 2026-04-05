# UI Components

Reusable patterns for building widget UIs. All components use CSS custom
properties from `core.md` and color ramps from `colors.md`.

## Cards

Container for grouped content:

```html
<div style="
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
">
  <div style="font-size:14px; font-weight:600; color:var(--text-primary)">
    Card Title
  </div>
  <div style="font-size:14px; color:var(--text-secondary)">
    Card body text.
  </div>
</div>
```

Variants:
- **Default:** `border: 1px solid var(--border)`
- **Highlighted:** `border: 1px solid var(--primary); background: var(--primary-bg)`
- **Grouped cards:** Use a parent `display:grid; grid-template-columns:repeat(auto-fit,minmax(200px,1fr)); gap:12px`

## Buttons

```css
.btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 16px;
  font-size: 14px;
  font-weight: 500;
  border-radius: 6px;
  border: 1px solid transparent;
  cursor: pointer;
  transition: all 0.15s ease;
  font-family: inherit;
}
.btn-primary {
  background: var(--primary);
  color: #fff;
  border-color: var(--primary);
}
.btn-primary:hover { opacity: 0.9; }
.btn-primary:active { opacity: 0.8; }
.btn-secondary {
  background: var(--surface);
  color: var(--text-primary);
  border-color: var(--border);
}
.btn-secondary:hover { background: var(--border); }
.btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
```

## Metric Cards

For displaying KPIs or statistics:

```html
<div style="
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 12px;
">
  <div style="
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 16px;
    text-align: center;
  ">
    <div style="font-size:24px; font-weight:600; color:var(--primary)">42</div>
    <div style="font-size:12px; color:var(--text-secondary); margin-top:4px">
      Active Users
    </div>
  </div>
</div>
```

## Form Elements

### Text Input

```css
.input {
  width: 100%;
  padding: 8px 12px;
  font-size: 14px;
  font-family: inherit;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent;
  color: var(--text-primary);
  outline: none;
  transition: border-color 0.15s ease;
}
.input:focus {
  border-color: var(--primary);
}
```

### Select

```css
.select {
  appearance: none;
  padding: 8px 32px 8px 12px;
  font-size: 14px;
  font-family: inherit;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent url("data:image/svg+xml,...") no-repeat right 10px center;
  color: var(--text-primary);
  cursor: pointer;
}
```

### Slider / Range

```css
.range {
  width: 100%;
  accent-color: var(--primary);
}
```

Pair with a value label:
```html
<div style="display:flex; align-items:center; gap:12px">
  <input type="range" class="range" min="0" max="100" value="50" id="slider">
  <span id="slider-val" style="font-size:14px; color:var(--text-primary); min-width:3ch">50</span>
</div>
```

## Tables

```css
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 14px;
}
.table th {
  text-align: left;
  font-weight: 600;
  padding: 8px 12px;
  border-bottom: 2px solid var(--border);
  color: var(--text-primary);
}
.table td {
  padding: 8px 12px;
  border-bottom: 1px solid var(--border);
  color: var(--text-secondary);
}
.table tr:hover td {
  background: var(--surface);
}
```

## Tabs

```html
<div style="display:flex; border-bottom:1px solid var(--border); gap:0">
  <button class="tab active" data-tab="tab1">Tab 1</button>
  <button class="tab" data-tab="tab2">Tab 2</button>
</div>
<div id="tab1" class="tab-panel">Content 1</div>
<div id="tab2" class="tab-panel" style="display:none">Content 2</div>
```

```css
.tab {
  padding: 8px 16px;
  font-size: 14px;
  font-family: inherit;
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  color: var(--text-secondary);
  cursor: pointer;
}
.tab.active {
  color: var(--primary);
  border-bottom-color: var(--primary);
}
.tab:hover { color: var(--text-primary); }
```

## Skeleton / Loading

For content that loads after script execution (e.g., chart rendering):

```css
.skeleton {
  background: linear-gradient(90deg, var(--surface) 25%, var(--border) 50%, var(--surface) 75%);
  background-size: 200% 100%;
  animation: shimmer 1.5s infinite;
  border-radius: 4px;
}
@keyframes shimmer {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}
```

Use skeletons as placeholders for charts/canvases that render via script:

```html
<div class="skeleton" id="chart-placeholder" style="height:300px; border-radius:8px"></div>
<canvas id="chart" style="display:none; width:100%; height:300px"></canvas>
```

In the script, hide the skeleton and show the canvas once the chart renders.

## Layout Patterns

### Two-column with sidebar

```html
<div style="display:grid; grid-template-columns:200px 1fr; gap:16px">
  <aside>Sidebar</aside>
  <main>Main content</main>
</div>
```

### Header + body + footer

```html
<div style="display:flex; flex-direction:column; gap:16px">
  <header>...</header>
  <div style="flex:1">Body</div>
  <footer style="font-size:12px; color:var(--text-secondary)">...</footer>
</div>
```

### Badge

```html
<span style="
  display: inline-block;
  padding: 2px 8px;
  font-size: 12px;
  font-weight: 500;
  border-radius: 4px;
  background: var(--primary-bg);
  color: var(--primary);
">Badge</span>
```
