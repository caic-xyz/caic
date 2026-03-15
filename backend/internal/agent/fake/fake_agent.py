# Fake agent that cycles through jokes, emitting Claude Code streaming JSON.
#
# Reads NDJSON from stdin (one prompt per line), responds with streaming text
# deltas followed by complete assistant and result messages. Exits on EOF.
# Used by the caic -tags e2e server for e2e testing.

import json
import sys
import time

JOKES = [
    "Why do programmers prefer dark mode? Because light attracts bugs.",
    "A SQL query walks into a bar, sees two tables, and asks: Can I JOIN you?",
    "Why do Java developers wear glasses? Because they can not C#.",
    "How many programmers does it take to change a light bulb? None, that is a hardware problem.",
    "There are only 10 types of people: those who understand binary and those who do not.",
    "A programmer puts two glasses on his bedside table before going to sleep."
    " A full one, in case he gets thirsty, and an empty one, in case he does not.",
]

PLAN_CONTENT = """## Fix authentication token validation

1. **Read** `backend/internal/auth/middleware.go` to understand the current token validation flow
2. **Write a failing test** in `middleware_test.go` that reproduces the expiry bug
3. **Fix** the timestamp comparison in `validateToken()`
   — use `time.Now().After(expiry)` instead of `Before(expiry)`
4. **Run** `go test ./backend/internal/auth/...` to verify the fix
5. **Update** the token refresh logic to handle clock skew (±30s tolerance)
"""

WIDGET_HTML = (
    "<style>\n"
    "  :root {\n"
    "    --text-primary: #1a1a2e;\n"
    "    --text-secondary: #555;\n"
    "    --border: #d0d0d0;\n"
    "    --surface: rgba(0,0,0,0.03);\n"
    "    --sky-top: #c9e8f7;\n"
    "    --sky-bot: #e8f4fb;\n"
    "    --water-top: #1a6fa8;\n"
    "    --water-bot: #0a3d6b;\n"
    "    --ray-color: #ffd700;\n"
    "    --normal-color: #aaa;\n"
    "    --angle-inc: #e55;\n"
    "    --angle-ref: #4a9;\n"
    "    --formula-bg: rgba(0,0,0,0.04);\n"
    "  }\n"
    "  @media (prefers-color-scheme: dark) {\n"
    "    :root {\n"
    "      --text-primary: #e0e0e0;\n"
    "      --text-secondary: #999;\n"
    "      --border: #333;\n"
    "      --surface: rgba(255,255,255,0.05);\n"
    "      --sky-top: #0d1f2d;\n"
    "      --sky-bot: #1a3344;\n"
    "      --water-top: #0a3d6b;\n"
    "      --water-bot: #041c30;\n"
    "      --formula-bg: rgba(255,255,255,0.06);\n"
    "    }\n"
    "  }\n"
    "  * { box-sizing: border-box; margin: 0; padding: 0; }\n"
    "  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }\n"
    "  .widget { padding: 16px; max-width: 640px; margin: 0 auto; }\n"
    "  h2 { font-size: 18px; font-weight: 600; color: var(--text-primary); margin-bottom: 4px; }\n"
    "  .subtitle { font-size: 13px; color: var(--text-secondary); margin-bottom: 16px; }\n"
    "  .diagram-wrap { width: 100%; border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }\n"
    "  .controls { margin-top: 14px; display: flex; align-items: center; gap: 12px; }\n"
    "  .controls label { font-size: 14px; font-weight: 600; color: var(--text-primary); white-space: nowrap; }\n"
    "  input[type=range] { flex: 1; accent-color: var(--angle-inc); cursor: pointer; }\n"
    "  .angle-val { font-size: 14px; font-weight: 600; color: var(--angle-inc); min-width: 36px; text-align: right; }\n"
    "  .formula-box { margin-top: 14px; background: var(--formula-bg); border: 1px solid var(--border);\n"
    "    border-radius: 8px; padding: 12px 16px; }\n"
    "  .formula-title { font-size: 12px; font-weight: 600; color: var(--text-secondary);\n"
    "    text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 8px; }\n"
    "  .formula-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }\n"
    "  .formula-text { font-family: 'SFMono-Regular', Consolas, monospace; font-size: 14px;\n"
    "    color: var(--text-primary); }\n"
    "  .val-inc { color: var(--angle-inc); font-weight: 600; }\n"
    "  .val-ref { color: var(--angle-ref); font-weight: 600; }\n"
    "  .explanation { margin-top: 12px; font-size: 13px; color: var(--text-secondary);\n"
    "    line-height: 1.6; border-top: 1px solid var(--border); padding-top: 12px; }\n"
    "</style>\n"
    "\n"
    '<div class="widget">\n'
    "  <h2>Light Refraction in Water</h2>\n"
    '  <p class="subtitle">Snell\'s Law: how light bends at the air\u2013water interface</p>\n'
    "\n"
    '  <div class="diagram-wrap">\n'
    '    <svg id="diagram" viewBox="0 0 500 340" style="width:100%;height:auto;display:block;">\n'
    "      <defs>\n"
    '        <linearGradient id="skyGrad" x1="0" y1="0" x2="0" y2="1">\n'
    '          <stop offset="0%" stop-color="var(--sky-top)"/>\n'
    '          <stop offset="100%" stop-color="var(--sky-bot)"/>\n'
    "        </linearGradient>\n"
    '        <linearGradient id="waterGrad" x1="0" y1="0" x2="0" y2="1">\n'
    '          <stop offset="0%" stop-color="var(--water-top)"/>\n'
    '          <stop offset="100%" stop-color="var(--water-bot)"/>\n'
    "        </linearGradient>\n"
    '        <marker id="arrowY" viewBox="0 0 10 7" refX="9" refY="3.5"\n'
    '          markerWidth="7" markerHeight="5" orient="auto-start-reverse">\n'
    '          <polygon points="0 0, 10 3.5, 0 7" fill="#ffd700"/>\n'
    "        </marker>\n"
    "      </defs>\n"
    '      <rect x="0" y="0" width="500" height="170" fill="url(#skyGrad)"/>\n'
    '      <rect x="0" y="170" width="500" height="170" fill="url(#waterGrad)"/>\n'
    '      <line x1="0" y1="170" x2="500" y2="170" stroke="rgba(255,255,255,0.4)" stroke-width="1.5"/>\n'
    '      <text x="14" y="24" font-size="13" fill="var(--text-secondary)"\n'
    '        font-family="-apple-system,sans-serif">AIR  n\u2081 = 1.00</text>\n'
    '      <text x="14" y="200" font-size="13" fill="rgba(255,255,255,0.7)"\n'
    '        font-family="-apple-system,sans-serif">WATER  n\u2082 = 1.33</text>\n'
    '      <line id="normalLine" x1="250" y1="40" x2="250" y2="300"\n'
    '        stroke="var(--normal-color)" stroke-width="1" stroke-dasharray="5,4"/>\n'
    '      <line id="incidentRay" stroke="#ffd700" stroke-width="2.5" marker-end="url(#arrowY)"/>\n'
    '      <line id="refractedRay" stroke="#ffd700" stroke-width="2.5" marker-end="url(#arrowY)"/>\n'
    '      <path id="arcInc" fill="none" stroke="var(--angle-inc)" stroke-width="1.5"/>\n'
    '      <path id="arcRef" fill="none" stroke="var(--angle-ref)" stroke-width="1.5"/>\n'
    '      <text id="lblInc" font-size="13" font-weight="600" fill="var(--angle-inc)"\n'
    '        font-family="-apple-system,sans-serif"/>\n'
    '      <text id="lblRef" font-size="13" font-weight="600" fill="var(--angle-ref)"\n'
    '        font-family="-apple-system,sans-serif"/>\n'
    "    </svg>\n"
    "  </div>\n"
    "\n"
    '  <div class="controls">\n'
    "    <label>Angle of incidence</label>\n"
    '    <input type="range" id="slider" min="0" max="89" value="40"/>\n'
    '    <span class="angle-val" id="sliderVal">40\u00b0</span>\n'
    "  </div>\n"
    "\n"
    '  <div class="formula-box">\n'
    '    <div class="formula-title">Snell\'s Law</div>\n'
    '    <div class="formula-row">\n'
    '      <span class="formula-text">n\u2081 \u00b7 sin(<span class="val-inc" id="fTheta1">40.0\u00b0</span>)'
    ' = n\u2082 \u00b7 sin(<span class="val-ref" id="fTheta2">28.9\u00b0</span>)</span>\n'
    '      <span class="formula-text" style="color:var(--text-secondary)">\u2192</span>\n'
    '      <span class="formula-text">1.00 \u00b7 <span class="val-inc" id="fSin1">0.6428</span>'
    ' = 1.33 \u00b7 <span class="val-ref" id="fSin2">0.4833</span></span>\n'
    "    </div>\n"
    "  </div>\n"
    "\n"
    '  <div class="explanation">\n'
    "    Light travels slower in water (speed \u2248 c/1.33) than in air (speed = c).\n"
    "    When a wavefront hits the surface at an angle, the part entering water slows first,\n"
    "    bending the ray toward the normal. The greater the speed difference (higher refractive\n"
    "    index), the sharper the bend. At <strong>0\u00b0</strong> (straight down) there is no\n"
    "    bending. Beyond the <em>critical angle</em> (~48.8\u00b0 from water side), total\n"
    "    internal reflection occurs.\n"
    "  </div>\n"
    "</div>\n"
    "\n"
    "<script>\n"
    "(function() {\n"
    "  const CX = 250, CY = 170;\n"
    "  const N1 = 1.0, N2 = 1.33;\n"
    "  const slider = document.getElementById('slider');\n"
    "  const sliderVal = document.getElementById('sliderVal');\n"
    "  const incidentRay = document.getElementById('incidentRay');\n"
    "  const refractedRay = document.getElementById('refractedRay');\n"
    "  const arcInc = document.getElementById('arcInc');\n"
    "  const arcRef = document.getElementById('arcRef');\n"
    "  const lblInc = document.getElementById('lblInc');\n"
    "  const lblRef = document.getElementById('lblRef');\n"
    "  const fTheta1 = document.getElementById('fTheta1');\n"
    "  const fTheta2 = document.getElementById('fTheta2');\n"
    "  const fSin1 = document.getElementById('fSin1');\n"
    "  const fSin2 = document.getElementById('fSin2');\n"
    "\n"
    "  function arcPath(cx, cy, r, startAngle, endAngle) {\n"
    "    const x1 = cx + r * Math.cos(startAngle);\n"
    "    const y1 = cy + r * Math.sin(startAngle);\n"
    "    const x2 = cx + r * Math.cos(endAngle);\n"
    "    const y2 = cy + r * Math.sin(endAngle);\n"
    "    const large = Math.abs(endAngle - startAngle) > Math.PI ? 1 : 0;\n"
    "    return `M ${x1} ${y1} A ${r} ${r} 0 ${large} 1 ${x2} ${y2}`;\n"
    "  }\n"
    "\n"
    "  function update() {\n"
    "    const deg1 = parseFloat(slider.value);\n"
    "    const rad1 = deg1 * Math.PI / 180;\n"
    "    const sinR = (N1 / N2) * Math.sin(rad1);\n"
    "    const rad2 = Math.asin(Math.min(sinR, 1));\n"
    "    const deg2 = rad2 * 180 / Math.PI;\n"
    "    sliderVal.textContent = deg1 + '\\u00b0';\n"
    "    const L1 = 130, L2 = 130;\n"
    "    const ix1 = CX - L1 * Math.sin(rad1);\n"
    "    const iy1 = CY - L1 * Math.cos(rad1);\n"
    "    const ix2 = CX - 8 * Math.sin(rad1);\n"
    "    const iy2 = CY - 8 * Math.cos(rad1);\n"
    "    incidentRay.setAttribute('x1', ix1); incidentRay.setAttribute('y1', iy1);\n"
    "    incidentRay.setAttribute('x2', ix2); incidentRay.setAttribute('y2', iy2);\n"
    "    const rx2 = CX + L2 * Math.sin(rad2);\n"
    "    const ry2 = CY + L2 * Math.cos(rad2);\n"
    "    const rx1 = CX + 8 * Math.sin(rad2);\n"
    "    const ry1 = CY + 8 * Math.cos(rad2);\n"
    "    refractedRay.setAttribute('x1', rx1); refractedRay.setAttribute('y1', ry1);\n"
    "    refractedRay.setAttribute('x2', rx2); refractedRay.setAttribute('y2', ry2);\n"
    "    if (deg1 > 2) {\n"
    "      const arcR1 = 40;\n"
    "      const incDirAngle = Math.atan2(-Math.cos(rad1), -Math.sin(rad1));\n"
    "      const normalUpAngle = -Math.PI / 2;\n"
    "      arcInc.setAttribute('d', arcPath(CX, CY, arcR1, normalUpAngle, incDirAngle));\n"
    "      const midAngle1 = (normalUpAngle + incDirAngle) / 2;\n"
    "      lblInc.setAttribute('x', CX + (arcR1 + 14) * Math.cos(midAngle1));\n"
    "      lblInc.setAttribute('y', CY + (arcR1 + 14) * Math.sin(midAngle1) + 4);\n"
    "      lblInc.setAttribute('text-anchor', 'middle');\n"
    "      lblInc.textContent = '\\u03b8\\u2081=' + deg1 + '\\u00b0';\n"
    "    } else { arcInc.setAttribute('d', ''); lblInc.textContent = ''; }\n"
    "    if (deg2 > 2) {\n"
    "      const arcR2 = 48;\n"
    "      const refDirAngle = Math.atan2(Math.cos(rad2), Math.sin(rad2));\n"
    "      const normalDownAngle = Math.PI / 2;\n"
    "      arcRef.setAttribute('d', arcPath(CX, CY, arcR2, normalDownAngle, refDirAngle));\n"
    "      const midAngle2 = (normalDownAngle + refDirAngle) / 2;\n"
    "      lblRef.setAttribute('x', CX + (arcR2 + 14) * Math.cos(midAngle2));\n"
    "      lblRef.setAttribute('y', CY + (arcR2 + 14) * Math.sin(midAngle2) + 4);\n"
    "      lblRef.setAttribute('text-anchor', 'middle');\n"
    "      lblRef.setAttribute('fill', 'rgba(255,255,255,0.9)');\n"
    "      lblRef.textContent = '\\u03b8\\u2082=' + deg2.toFixed(1) + '\\u00b0';\n"
    "    } else { arcRef.setAttribute('d', ''); lblRef.textContent = ''; }\n"
    "    fTheta1.textContent = deg1.toFixed(1) + '\\u00b0';\n"
    "    fTheta2.textContent = deg2.toFixed(1) + '\\u00b0';\n"
    "    fSin1.textContent = Math.sin(rad1).toFixed(4);\n"
    "    fSin2.textContent = Math.sin(rad2).toFixed(4);\n"
    "  }\n"
    "  slider.addEventListener('input', update);\n"
    "  update();\n"
    "})();\n"
    "</script>"
)
WIDGET_TITLE = "light_refraction_in_water"

ASK_QUESTION = {
    "question": "The rate limiter needs a storage backend. Which approach should I use?",
    "options": [
        {
            "label": "In-memory (sync.Map)",
            "description": "Simple, no dependencies. Lost on restart.",
        },
        {
            "label": "Redis",
            "description": "Shared across instances, persists across restarts.",
        },
        {
            "label": "SQLite",
            "description": "Persistent, no external service. Slightly slower.",
        },
    ],
}

# Realistic demo scenarios triggered by FAKE_DEMO keyword.
# Each scenario is a list of emissions (text + tool_uses).
DEMO_SCENARIOS = [
    {
        "steps": [
            {
                "text": "I'll investigate the authentication issue. Let me read the middleware code first.",
            },
            {
                "tool": ("toolu_read_1", "Read", {"file_path": "backend/internal/auth/middleware.go"}),
            },
            {
                "text": (
                    "Found the bug. The token expiry check on line 47 is using"
                    " `time.Now().Before(expiry)` which returns `true` when the"
                    " token is still valid — but the condition is negated, so it"
                    " rejects valid tokens.\n\nLet me write a test first, then fix it."
                ),
            },
            {
                "tool": (
                    "toolu_edit_1",
                    "Edit",
                    {
                        "file_path": "backend/internal/auth/middleware_test.go",
                        "old_string": "func TestValidateToken(t *testing.T) {",
                        "new_string": (
                            "func TestValidateToken_Expiry(t *testing.T) {\n"
                            "\ttoken := createTestToken(time.Now().Add(time.Hour))\n"
                            "\tif err := validateToken(token); err != nil {\n"
                            '\t\tt.Fatalf("valid token rejected: %v", err)\n'
                            "\t}\n"
                            "}\n\n"
                            "func TestValidateToken(t *testing.T) {"
                        ),
                    },
                ),
            },
            {
                "tool": (
                    "toolu_edit_2",
                    "Edit",
                    {
                        "file_path": "backend/internal/auth/middleware.go",
                        "old_string": "if !time.Now().Before(claims.ExpiresAt) {",
                        "new_string": "if time.Now().After(claims.ExpiresAt) {",
                    },
                ),
            },
            {
                "tool": ("toolu_bash_1", "Bash", {"command": "cd /workspace && go test ./backend/internal/auth/..."}),
            },
        ],
        "result": (
            "Fixed the token validation bug. The expiry check was using"
            " `time.Before` with inverted logic. Added a regression test."
        ),
        "cost": 0.03,
        "duration": 12400,
    },
    {
        "steps": [
            {
                "text": "I'll add rate limiting to the API. Let me check the current server setup.",
            },
            {
                "tool": ("toolu_read_2", "Read", {"file_path": "backend/internal/server/server.go"}),
            },
            {
                "text": (
                    "The server uses a standard `http.ServeMux`. I'll create a"
                    " middleware that wraps it with a token bucket rate limiter.\n\n"
                    "```go\ntype rateLimiter struct {\n"
                    "\tmu      sync.Mutex\n"
                    "\tbuckets map[string]*bucket\n"
                    "\trate    float64\n"
                    "\tburst   int\n}\n```"
                ),
            },
            {
                "tool": (
                    "toolu_write_1",
                    "Write",
                    {
                        "file_path": "backend/internal/server/ratelimit.go",
                        "content": (
                            "package server\n\n"
                            'import (\n\t"net/http"\n\t"sync"\n\t"time"\n)\n\n'
                            "type rateLimiter struct {\n"
                            "\tmu      sync.Mutex\n"
                            "\tbuckets map[string]*bucket\n"
                            "\trate    float64\n"
                            "\tburst   int\n}\n\n"
                            "func newRateLimiter(rate float64, burst int) *rateLimiter {\n"
                            "\treturn &rateLimiter{\n"
                            "\t\tbuckets: make(map[string]*bucket),\n"
                            "\t\trate: rate, burst: burst,\n"
                            "\t}\n}\n\n"
                            "func (rl *rateLimiter) Wrap(next http.Handler) http.Handler {\n"
                            "\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n"
                            "\t\tip := r.RemoteAddr\n"
                            "\t\tif !rl.allow(ip) {\n"
                            '\t\t\thttp.Error(w, "rate limit exceeded", http.StatusTooManyRequests)\n'
                            "\t\t\treturn\n"
                            "\t\t}\n"
                            "\t\tnext.ServeHTTP(w, r)\n"
                            "\t})\n}\n"
                        ),
                    },
                ),
            },
            {
                "tool": (
                    "toolu_bash_2",
                    "Bash",
                    {"command": "cd /workspace && go test ./backend/internal/server/... -count=1"},
                ),
            },
        ],
        "result": (
            "Added token bucket rate limiter (100 req/s burst 200 per IP) as HTTP middleware wrapping all API routes."
        ),
        "cost": 0.05,
        "duration": 18700,
    },
    {
        "steps": [
            {
                "text": "Let me review the CI configuration and optimize the test pipeline.",
            },
            {
                "tool": ("toolu_read_3", "Read", {"file_path": ".github/workflows/test.yml"}),
            },
            {
                "text": (
                    "The tests are running sequentially which is slow. I'll split them"
                    " into a matrix strategy:\n\n"
                    "- **Unit tests**: Go + frontend + Python (parallel)\n"
                    "- **Lint**: golangci-lint + ESLint + ruff (parallel)\n"
                    "- **E2E**: Playwright tests (after build)\n\n"
                    "This should cut CI time from ~8min to ~3min."
                ),
            },
            {
                "tool": (
                    "toolu_edit_3",
                    "Edit",
                    {
                        "file_path": ".github/workflows/test.yml",
                        "old_string": "    steps:\n      - uses: actions/checkout@v4",
                        "new_string": (
                            "    strategy:\n"
                            "      matrix:\n"
                            "        target: [test-go, test-frontend, lint, e2e]\n"
                            "    steps:\n"
                            "      - uses: actions/checkout@v4"
                        ),
                    },
                ),
            },
            {
                "tool": ("toolu_bash_3", "Bash", {"command": "cd /workspace && actionlint .github/workflows/test.yml"}),
            },
        ],
        "result": "Parallelized CI pipeline using matrix strategy. Expected speedup: ~8min → ~3min.",
        "cost": 0.02,
        "duration": 8300,
    },
]


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def emit_text(text: str) -> None:
    """Emit streaming text deltas followed by the complete assistant message."""
    # Split roughly in half for two streaming deltas.
    mid = len(text) // 2
    sp = text.find(" ", mid)
    if sp == -1:
        sp = mid
    part1 = text[: sp + 1]
    part2 = text[sp + 1 :]
    emit(
        {
            "type": "stream_event",
            "event": {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": part1},
            },
        }
    )
    time.sleep(0.05)
    emit(
        {
            "type": "stream_event",
            "event": {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": part2},
            },
        }
    )
    emit(
        {
            "type": "assistant",
            "message": {
                "role": "assistant",
                "content": [{"type": "text", "text": text}],
            },
        }
    )


def emit_tool_use(tool_id: str, name: str, input_obj: dict) -> None:
    emit(
        {
            "type": "assistant",
            "message": {
                "role": "assistant",
                "content": [{"type": "tool_use", "id": tool_id, "name": name, "input": input_obj}],
            },
        }
    )


def emit_result(turns: int, result: str, cost: float = 0.01, duration: int = 500) -> None:
    emit(
        {
            "type": "result",
            "subtype": "success",
            "result": result,
            "num_turns": turns,
            "total_cost_usd": cost,
            "duration_ms": duration,
        }
    )


def emit_plan_turn(turns: int) -> None:
    """Emit Write(.claude/plans/plan.md) + ExitPlanMode + result."""
    emit_tool_use(
        "toolu_write_plan",
        "Write",
        {"file_path": ".claude/plans/plan.md", "content": PLAN_CONTENT},
    )
    emit_tool_use("toolu_exit_plan", "ExitPlanMode", {})
    emit_result(turns, "Plan created")


def emit_widget_turn(turns: int) -> None:
    """Emit show_widget streaming (content_block_start + input_json_delta + content_block_stop) + final + result."""
    widget_input = json.dumps({"widget_code": WIDGET_HTML, "title": WIDGET_TITLE})
    # Stream content_block_start
    emit(
        {
            "type": "stream_event",
            "event": {
                "type": "content_block_start",
                "index": 0,
                "content_block": {"type": "tool_use", "id": "toolu_widget", "name": "show_widget"},
            },
        }
    )
    # Stream partial JSON deltas
    mid = len(widget_input) // 2
    emit(
        {
            "type": "stream_event",
            "event": {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "input_json_delta", "partial_json": widget_input[:mid]},
            },
        }
    )
    time.sleep(0.05)
    emit(
        {
            "type": "stream_event",
            "event": {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "input_json_delta", "partial_json": widget_input[mid:]},
            },
        }
    )
    # Stream content_block_stop
    emit(
        {
            "type": "stream_event",
            "event": {"type": "content_block_stop", "index": 0},
        }
    )
    # Final assistant message with the widget tool_use block.
    emit_tool_use("toolu_widget", "show_widget", {"widget_code": WIDGET_HTML, "title": WIDGET_TITLE})
    # Tool result (widget rendering is async).
    emit(
        {
            "type": "user",
            "message": {"content": [{"type": "text", "text": "Widget rendered"}], "is_error": False},
            "parent_tool_use_id": "toolu_widget",
        }
    )
    emit_result(turns, "Widget displayed")


def emit_ask_turn(turns: int) -> None:
    """Emit AskUserQuestion + result."""
    emit_tool_use(
        "toolu_ask",
        "AskUserQuestion",
        {"questions": [ASK_QUESTION]},
    )
    emit_result(turns, "Asking user")


def emit_demo_turn(turns: int) -> None:
    """Emit a realistic multi-tool scenario."""
    scenario = DEMO_SCENARIOS[(turns - 1) % len(DEMO_SCENARIOS)]
    for step in scenario["steps"]:
        if "text" in step:
            emit_text(step["text"])
            time.sleep(0.1)
        if "tool" in step:
            tool_id, name, input_obj = step["tool"]
            emit_tool_use(tool_id, name, input_obj)
            time.sleep(0.15)
    emit_result(turns, scenario["result"], scenario.get("cost", 0.01), scenario.get("duration", 500))


def main() -> None:
    # System init before first prompt.
    emit(
        {
            "type": "system",
            "subtype": "init",
            "session_id": "test-session",
            "cwd": "/workspace",
            "model": "fake-model",
            "claude_code_version": "0.0.0-test",
        }
    )

    turns = 0
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            continue
        turns += 1

        # Exact keyword triggers (for e2e tests).
        if "FAKE_PLAN" in line:
            emit_plan_turn(turns)
            continue
        if "FAKE_ASK" in line:
            emit_ask_turn(turns)
            continue
        if "FAKE_DEMO" in line:
            emit_demo_turn(turns)
            continue

        # Natural prompt detection (for screenshots with clean prompts).
        lower = line.lower()
        if any(w in lower for w in ("plan", "design", "architect", "outline")):
            emit_plan_turn(turns)
            continue
        if any(w in lower for w in ("which", "should i", "choose", "prefer")):
            emit_ask_turn(turns)
            continue
        if any(w in lower for w in ("fix", "bug", "refactor", "update", "add", "implement")):
            emit_demo_turn(turns)
            continue

        if "FAKE_WIDGET" in line:
            emit_widget_turn(turns)
            continue

        joke = JOKES[(turns - 1) % len(JOKES)]
        emit_text(joke)
        emit_result(turns, joke)


if __name__ == "__main__":
    main()
