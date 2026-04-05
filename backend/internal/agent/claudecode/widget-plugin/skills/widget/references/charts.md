# Chart Guidelines

## Chart.js Setup

Use Chart.js 4 from CDN. Always use `<canvas>`, not `<div>`:

```html
<canvas id="myChart" style="width:100%; max-height:400px"></canvas>

<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<script>
  const ctx = document.getElementById('myChart');
  new Chart(ctx, { /* config */ });
</script>
```

### Responsive Configuration

Always enable responsive mode and set `maintainAspectRatio`:

```javascript
new Chart(ctx, {
  type: 'bar',
  data: { /* ... */ },
  options: {
    responsive: true,
    maintainAspectRatio: true,
    aspectRatio: 2, // width:height ratio — use 2 for landscape, 1.5 for square-ish
  }
});
```

## Canvas Sizing

- Use CSS for sizing: `style="width:100%; max-height:400px"`
- Never set `width`/`height` HTML attributes — let Chart.js handle device pixel ratio.
- For dashboards with multiple charts, use `max-height: 300px` per chart.

## Chart Types & When to Use Them

| Type | Use for | Chart.js type |
|------|---------|---------------|
| Bar | Comparing categories | `bar` |
| Horizontal bar | Long category labels | `bar` + `indexAxis: 'y'` |
| Line | Trends over time | `line` |
| Area | Cumulative trends | `line` + `fill: true` |
| Doughnut | Part-of-whole (≤6 slices) | `doughnut` |
| Pie | Part-of-whole (≤4 slices) | `pie` |
| Scatter | Correlation between 2 variables | `scatter` |
| Radar | Multi-dimensional comparison | `radar` |

## Colors

Use the color ramps from `colors.md`. Map datasets to ramp stops:

```javascript
const colors = {
  purple: { bg: 'rgba(124,58,237,0.1)', border: '#7c3aed' },
  teal:   { bg: 'rgba(20,184,166,0.1)', border: '#14b8a6' },
  coral:  { bg: 'rgba(232,86,74,0.1)',  border: '#e8564a' },
  blue:   { bg: 'rgba(59,130,246,0.1)', border: '#3b82f6' },
  green:  { bg: 'rgba(34,197,94,0.1)',  border: '#22c55e' },
  amber:  { bg: 'rgba(245,158,11,0.1)', border: '#f59e0b' },
};
```

For dark mode detection in scripts:

```javascript
const isDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
const textColor = isDark ? '#e0e0e0' : '#1a1a2e';
const gridColor = isDark ? 'rgba(255,255,255,0.1)' : 'rgba(0,0,0,0.06)';
```

## Legend

- Position: `top` for ≤3 datasets, `right` for 4+ datasets.
- Use `pointStyle: 'circle'` for cleaner legend markers.
- Hide legend for single-dataset charts.

```javascript
options: {
  plugins: {
    legend: {
      position: 'top',
      labels: {
        usePointStyle: true,
        pointStyle: 'circle',
        color: textColor,
        font: { size: 12, family: "-apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif" },
      }
    }
  }
}
```

## Axes

```javascript
options: {
  scales: {
    x: {
      grid: { color: gridColor },
      ticks: {
        color: textColor,
        font: { size: 12 },
        maxRotation: 45,
      },
    },
    y: {
      grid: { color: gridColor },
      ticks: {
        color: textColor,
        font: { size: 12 },
        callback: (v) => new Intl.NumberFormat('en-US', { notation: 'compact' }).format(v),
      },
      beginAtZero: true,
    },
  },
}
```

## Number Formatting

Use `Intl.NumberFormat` for all displayed numbers:

```javascript
const fmt = new Intl.NumberFormat('en-US');
const fmtCompact = new Intl.NumberFormat('en-US', { notation: 'compact' });
const fmtCurrency = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' });
const fmtPercent = new Intl.NumberFormat('en-US', { style: 'percent', minimumFractionDigits: 1 });
```

## Tooltips

```javascript
options: {
  plugins: {
    tooltip: {
      backgroundColor: isDark ? '#333' : '#fff',
      titleColor: textColor,
      bodyColor: textColor,
      borderColor: isDark ? '#555' : '#e0e0e0',
      borderWidth: 1,
      cornerRadius: 6,
      padding: 10,
      bodyFont: { size: 13 },
      callbacks: {
        label: (ctx) => `${ctx.dataset.label}: ${fmt.format(ctx.parsed.y)}`,
      },
    },
  },
}
```

## Dashboard Layouts

For multi-chart dashboards, use a grid:

```html
<div style="display:grid; grid-template-columns:repeat(auto-fit,minmax(300px,1fr)); gap:16px">
  <div style="border:1px solid var(--border); border-radius:8px; padding:16px">
    <div style="font-size:14px; font-weight:600; margin-bottom:12px; color:var(--text-primary)">
      Chart Title
    </div>
    <canvas id="chart1" style="width:100%; max-height:300px"></canvas>
  </div>
  <!-- more chart cards -->
</div>
```

## Animation

Disable animations for streaming (content appears progressively, animation
would replay on each DOM update):

```javascript
options: {
  animation: false,
}
```

If the widget is non-streaming (fully rendered by script), enable short
animations:

```javascript
options: {
  animation: {
    duration: 600,
    easing: 'easeOutQuart',
  },
}
```

## Skeleton Placeholder

Show a skeleton while the chart loads from CDN:

```html
<div class="skeleton" id="chart-skeleton" style="height:300px; border-radius:8px"></div>
<canvas id="chart" style="display:none; width:100%; max-height:400px"></canvas>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<script>
  document.getElementById('chart-skeleton').style.display = 'none';
  const canvas = document.getElementById('chart');
  canvas.style.display = 'block';
  new Chart(canvas, { /* ... */ });
</script>
```
