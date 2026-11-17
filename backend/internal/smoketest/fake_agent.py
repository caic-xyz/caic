# Fake agent emits deterministic e2e scenarios with input gates, ordered message identity, and fixture time.
#
# Each envelope's message is a flat object whose "type" field maps directly to
# an agent.Message type (init, text, text_delta, tool_use, ask, widget,
# widget_delta, tool_result, native_subagent, background_command, turn_started,
# result).
#
# Reads plain text from stdin (one prompt per line), responds with streaming
# text deltas followed by complete messages.  Exits on EOF or null-byte
# sentinel.  Used by the caic -tags e2e server for e2e testing.

import json
import os
import re
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

NATURAL_ASK_RE = re.compile(r"\b(?:which|should i|choose|prefer)\b")
NATURAL_DEMO_RE = re.compile(r"\b(?:fix|bug|refactor|update|add|implement)\b")
NATURAL_NATIVE_RE = re.compile(r"\bparallel subagents?\b")
NATURAL_BACKGROUND_RE = re.compile(r"\bbackground commands?\b")
NATURAL_PLAN_RE = re.compile(r"\b(?:plan|design|architect|outline)\b")

TOOL_DURATIONS_MS = {
    "Read": 180,
    "Edit": 240,
    "Write": 310,
    "Bash": 620,
}

LIFECYCLE_RESULT = "Lifecycle completed"
LIFECYCLE_STREAM_MARKER = "Lifecycle streaming marker"

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
            {"thinking": "Inspecting the authentication flow before making changes."},
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
        "duration": 2200,
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
        "duration": 1900,
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
        "duration": 1500,
    },
]


last_timestamp_ms = 0


def emit(obj: dict) -> None:
    # Task logs are v2: native harness output is wrapped in the canonical
    # envelope before the Go reader validates and records it.
    global last_timestamp_ms
    # Distinct fixture timestamps preserve transcript anchors even when the
    # entire scenario is written in one batch without real-time delays.
    timestamp_ms = max(int(time.time() * 1000), last_timestamp_ms + 1)
    last_timestamp_ms = timestamp_ms
    envelope = '{{"t":"agent","ts":{}.{:03d},"msg":{}}}'.format(
        timestamp_ms // 1000, timestamp_ms % 1000, json.dumps(obj, separators=(",", ":"))
    )
    sys.stdout.write(envelope + "\n")
    sys.stdout.flush()


def advance_clock(ms: int) -> None:
    """Move the timestamps of later messages forward without waiting."""
    global last_timestamp_ms
    last_timestamp_ms = max(int(time.time() * 1000), last_timestamp_ms) + ms


def emit_text(text: str) -> None:
    """Emit streaming text deltas followed by the complete text message."""
    # Split roughly in half for two streaming deltas.
    mid = len(text) // 2
    sp = text.find(" ", mid)
    if sp == -1:
        sp = mid
    part1 = text[: sp + 1]
    part2 = text[sp + 1 :]
    emit({"type": "text_delta", "text": part1})
    emit({"type": "text_delta", "text": part2})
    emit({"type": "text", "text": text})


def emit_tool_use(tool_id: str, name: str, input_obj: dict) -> None:
    emit({"type": "tool_use", "id": tool_id, "name": name, "input": input_obj})


def emit_tool_result(tool_id: str, duration_ms: int) -> None:
    emit({"type": "tool_result", "tool_use_id": tool_id, "duration_ms": duration_ms})


def emit_result(turns: int, result: str, cost: float = 0.01, duration: int = 500) -> None:
    visual = os.environ.get("CAIC_E2E_VISUALS") == "1"
    if visual:
        cost, duration = {
            "Plan created": (0.015, 3400),
            "Widget displayed": (0.012, 1400),
            "Asking user": (0.008, 900),
            "Native activity finished": (0.024, 2600),
        }.get(result, (cost, duration))
    emit(
        {
            "type": "result",
            "subtype": "success",
            "result": result,
            "num_turns": turns,
            "total_cost_usd": round(cost * 17, 2) if visual else cost,
            "duration_ms": duration * 25 if visual else duration,
            "duration_api_ms": duration * 18 if visual else 0,
            "context_window": 200000 if visual else 0,
            "usage": {
                "input_tokens": 2400 + turns * 300,
                "output_tokens": 3800 + turns * 400,
                "cache_creation_input_tokens": 8400,
                "cache_read_input_tokens": 42000,
                "reasoning_output_tokens": 1200,
            }
            if visual
            else {},
        }
    )


def emit_capture_running() -> None:
    """Emit useful work and hold an active verification tool until shutdown."""
    emit({"type": "thinking", "text": "Checking request identity and existing API middleware before adding limits."})
    emit(
        {
            "type": "text",
            "text": "Checking the API middleware, applying rate limits, and running the focused regression tests.",
        }
    )
    emit_tool_use("toolu_rate_read", "Read", {"file_path": "backend/internal/server/router.go"})
    emit_tool_result("toolu_rate_read", 180)
    emit_tool_use(
        "toolu_rate_edit",
        "Edit",
        {
            "file_path": "backend/internal/server/ratelimit.go",
            "old_string": "const maxRateLimitBuckets = 100_000",
            "new_string": "const maxRateLimitBuckets = 50_000",
        },
    )
    emit_tool_result("toolu_rate_edit", 240)
    emit_tool_use("toolu_rate_verify", "Bash", {"command": "go test ./backend/internal/server -run TestRateLimit"})
    # Reading the normal input channel is the cancellation boundary. No timer,
    # completion result, or subsequent prompt can settle this capture task.
    for line in sys.stdin:
        if "\x00" in line:
            break


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
    """Emit streaming widget deltas, final widget message, tool result, and result."""
    # Stream partial HTML deltas.
    mid = len(WIDGET_HTML) // 2
    emit({"type": "widget_delta", "id": "toolu_widget", "delta": WIDGET_HTML[:mid]})
    emit({"type": "widget_delta", "id": "toolu_widget", "delta": WIDGET_HTML[mid:]})
    # Final complete widget.
    emit({"type": "widget", "id": "toolu_widget", "title": WIDGET_TITLE, "html": WIDGET_HTML})
    # Tool result.
    emit({"type": "tool_result", "tool_use_id": "toolu_widget"})
    emit_result(turns, "Widget displayed")


def emit_ask_turn(turns: int) -> None:
    """Emit AskUserQuestion + result."""
    emit({"type": "ask", "id": "toolu_ask", "questions": [ASK_QUESTION]})
    emit_result(turns, "Asking user")


def emit_demo_turn(turns: int, gated: bool) -> None:
    """Emit a realistic multi-tool scenario."""
    scenario = DEMO_SCENARIOS[(turns - 1) % len(DEMO_SCENARIOS)]
    for step in scenario["steps"]:
        if "thinking" in step:
            emit({"type": "thinking", "text": step["thinking"]})
        if "text" in step:
            emit_text(step["text"])
        if "tool" in step:
            tool_id, name, input_obj = step["tool"]
            emit_tool_use(tool_id, name, input_obj)
            duration_ms = TOOL_DURATIONS_MS.get(name, 200)
            emit_tool_result(tool_id, duration_ms)
    if not gated or gate("FAKE_DEMO_FINISH"):
        emit_result(turns, scenario["result"], scenario.get("cost", 0.01), scenario.get("duration", 500))


def emit_lifecycle_turn(turns: int) -> None:
    """Emit the stable streaming and completion contract for lifecycle tests."""
    emit_text(LIFECYCLE_STREAM_MARKER)
    emit_result(turns, LIFECYCLE_RESULT)


def emit_quota_recovery_turn(turns: int) -> None:
    """Emit an active quota block for the guided recovery e2e flow."""
    emit_text("The current harness cannot continue until its quota resets.")
    emit(
        {
            "type": "rate_limit",
            "status": "rejected",
            "resets_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 3600)),
            "rate_limit_type": "five_hour",
            "utilization": 1,
            "quota_provider": "claudecode",
            "quota_label": "Claude Code",
            "quota_window": "5h",
        }
    )
    emit_result(turns, "Quota exhausted")


# Background work mirrors Claude Code task records as the claudecode adapter
# folds them: a backgrounded Bash and a Monitor start local_bash tasks, a
# backgrounded Agent starts a local_agent task. Labels are the tool
# descriptions; summaries follow task_notification. Claude starts a turn by
# itself to deliver a notification that arrives while it waits for the user,
# so such a terminal update follows a turn_started and precedes that turn's
# result. A notification during a turn joins it.
BG_CHECKS = {
    "tool": "toolu_bg_checks",
    "watch": False,
    "id": "claude:shell:bchk7x2qa",
    "description": "Run fix, verify, and test",
    "output": "/tmp/claude-1000/-workspace/test-session/tasks/bchk7x2qa.output",
}
BG_WATCH = {
    "tool": "toolu_bg_watch",
    "watch": True,
    "id": "claude:shell:bwch4m8re",
    "description": "fix/verify/test results",
    "output": "/tmp/claude-1000/-workspace/test-session/tasks/bwch4m8re.output",
}
BG_REVIEW = {
    "tool": "toolu_bg_review",
    "id": "claude:task:a6c1e0f4b9d27e835",
    "run": "0mwk1a2b3-5d6e7f80",
    "description": "Review the API handlers",
    # The subagent_type local Claude Code task logs report most often.
    "kind": "general-purpose",
    "prompt": "Review backend/internal/server for missing input validation. Report each finding with its location.",
}


def emit_tool_started(tool: str) -> None:
    # Claude Code reports no duration for a tool that only starts a task.
    emit({"type": "tool_result", "tool_use_id": tool})


def emit_background_bash(item: dict) -> None:
    emit_tool_use(
        item["tool"],
        "Bash",
        {
            "command": 'make fix verify test > /tmp/checks.log 2>&1; echo "exit=$?"',
            "description": item["description"],
            "timeout": 600000,
            "run_in_background": True,
        },
    )
    command = {"id": item["id"], "label": item["description"], "status": "running", "tool_use_id": item["tool"]}
    emit({"type": "background_command", "command": command})
    emit_tool_started(item["tool"])


def emit_monitor(item: dict) -> None:
    emit_tool_use(
        item["tool"],
        "Monitor",
        {
            "command": "tail -n +1 -f /tmp/checks.log | grep --line-buffered -E 'FAIL|exit='",
            "description": item["description"],
            "timeout_ms": 1800000,
        },
    )
    command = {
        "id": item["id"],
        "label": item["description"],
        "status": "running",
        "tool_use_id": item["tool"],
        "watch": item["watch"],
    }
    emit({"type": "background_command", "command": command})
    emit_tool_started(item["tool"])


def emit_command_end(item: dict, summary: str, exit_code: int | None) -> None:
    """Emit task_updated then task_notification, as the adapter folds them."""
    command = {
        "id": item["id"],
        "label": item["description"],
        "status": "completed",
        "tool_use_id": item["tool"],
        **({"watch": True} if item["watch"] else {}),
    }
    emit({"type": "background_command", "command": command})
    command = {**command, "result": summary, "output_ref": item["output"]}
    if exit_code is not None:
        command["exit_code"] = exit_code
    emit({"type": "background_command", "command": command})


def emit_background_turn(turns: int, gated: bool) -> None:
    """Start a command, a watch, and an agent that all outlive the result.

    Ungated, all three keep running. Gated, FAKE_BACKGROUND_SETTLE_WORK ends
    the command and the agent; the watch remains and the task waits.
    FAKE_BACKGROUND_SETTLE_WATCH ends the watch.
    """
    emit_text("Starting the checks in the background, with a watch on failures and a review agent.")
    emit_background_bash(BG_CHECKS)
    emit_monitor(BG_WATCH)
    emit_tool_use(
        BG_REVIEW["tool"],
        "Agent",
        {
            "description": BG_REVIEW["description"],
            "subagent_type": BG_REVIEW["kind"],
            "prompt": BG_REVIEW["prompt"],
            "run_in_background": True,
        },
    )
    subagent = {
        "run_id": BG_REVIEW["run"],
        "tool_use_id": BG_REVIEW["tool"],
        "id": BG_REVIEW["id"],
        "label": BG_REVIEW["description"],
        "kind": BG_REVIEW["kind"],
        "prompt": BG_REVIEW["prompt"],
        "status": "running",
        "background": True,
    }
    emit({"type": "native_subagent", "subagent": subagent})
    emit_tool_started(BG_REVIEW["tool"])
    # The card shows the background counts beside the context size.
    emit({"type": "usage", "usage": {}, "context_used": 45000, "context_window": 200000})
    emit_text("The checks, the watch, and the review run in the background. I will report when they finish.")
    emit_result(turns, "Background work started")
    if not gated or not gate("FAKE_BACKGROUND_SETTLE_WORK"):
        return
    emit({"type": "turn_started"})
    emit_text("The checks and the review are still running.")
    emit_result(turns, "Background work running")
    emit({"type": "turn_started"})
    emit_command_end(BG_CHECKS, f'Background command "{BG_CHECKS["description"]}" completed (exit code 0)', 0)
    emit_text("The checks passed with exit code 0.")
    emit_result(turns, "Checks passed")
    emit({"type": "turn_started"})
    emit({"type": "native_subagent", "subagent": {**subagent, "status": "completed"}})
    review = "The handlers validate every input. I found no missing checks."
    emit({"type": "native_subagent", "subagent": {**subagent, "status": "completed", "result": review}})
    emit_text("The review found no missing input validation.")
    emit_result(turns, "Review finished")
    if not gate("FAKE_BACKGROUND_SETTLE_WATCH"):
        return
    emit({"type": "turn_started"})
    emit_text("The watch is still open.")
    emit_result(turns, "Watch running")
    emit({"type": "turn_started"})
    emit_command_end(BG_WATCH, f'Monitor "{BG_WATCH["description"]}" stream ended', None)
    emit_text("The watch ended without a failure.")
    emit_result(turns, "Watch ended")


BG_EDGE_WATCH = {
    "tool": "toolu_edge_watch",
    "watch": True,
    "id": "claude:shell:bedge1wch",
    "description": "deploy log errors",
}
# The edge agent reports no subagent_type, so its chip keeps the generic kind.
BG_EDGE_AGENT = {
    "tool": "toolu_edge_agent",
    "id": "claude:task:edge7agent",
    "description": "Profile the build",
}


def emit_background_edges_turn(turns: int) -> None:
    """Start background work that the root transcript does not show directly.

    A command runs inside a background agent's transcript, a command has no
    launching call, and a watch runs in the root. A later compaction archives
    the watch's call. Only FAKE_BACKGROUND_EDGE_AGENT_DONE settles anything:
    the agent.
    """
    emit_text("Starting a watch on the deploy log and a background profiling agent.")
    emit_monitor(BG_EDGE_WATCH)
    emit_tool_use(
        BG_EDGE_AGENT["tool"],
        "Agent",
        {"description": BG_EDGE_AGENT["description"], "prompt": "Profile the build.", "run_in_background": True},
    )
    subagent = {
        "tool_use_id": BG_EDGE_AGENT["tool"],
        "id": BG_EDGE_AGENT["id"],
        "label": BG_EDGE_AGENT["description"],
        "prompt": "Profile the build.",
        "status": "running",
        "background": True,
    }
    emit({"type": "native_subagent", "subagent": subagent})
    emit_tool_started(BG_EDGE_AGENT["tool"])
    child = {"agentID": BG_EDGE_AGENT["id"]}
    emit({"type": "text", "text": "Profiling the build in the background.", "source": child})
    emit(
        {
            "type": "tool_use",
            "id": "toolu_edge_child",
            "name": "Bash",
            "input": {"command": "make profile", "description": "Profile build", "run_in_background": True},
            "source": child,
        }
    )
    command = {
        "id": "claude:shell:bedge2chd",
        "label": "Profile build",
        "status": "running",
        "tool_use_id": "toolu_edge_child",
    }
    emit({"type": "background_command", "command": command, "source": child})
    emit({"type": "tool_result", "tool_use_id": "toolu_edge_child", "source": child})
    emit(
        {
            "type": "background_command",
            "command": {"id": "claude:shell:bedge3orp", "label": "Detached cleanup", "status": "running"},
        }
    )
    emit_text("The watch, the agent, and the cleanup run in the background.")
    emit_result(turns, "Background edges started")


def emit_background_edge_agent_done_turn(turns: int) -> None:
    """Finish the edge agent while the command in its transcript keeps running."""
    subagent = {"id": BG_EDGE_AGENT["id"], "status": "completed", "result": "The build profile is ready."}
    emit({"type": "native_subagent", "subagent": subagent})
    emit_text("The profiling agent finished; its command still runs.")
    emit_result(turns, "Edge agent finished")


def emit_background_watch_only_turn(turns: int) -> None:
    """Settle a command during the turn and leave only a watch after the result."""
    emit_text("Starting the checks in the background, with a watch on failures.")
    emit_background_bash(BG_CHECKS)
    emit_monitor(BG_WATCH)
    emit_command_end(BG_CHECKS, f'Background command "{BG_CHECKS["description"]}" completed (exit code 0)', 0)
    emit_text("The checks passed with exit code 0. The watch stays open.")
    emit_result(turns, "Checks passed")


TIMELINE_READS = [
    f"backend/internal/server/{name}.go"
    for name in (
        "router",
        "middleware",
        "auth",
        "session",
        "task_handlers",
        "task_history_http",
        "usage_handlers",
        "config",
        "errors",
        "events",
        "metrics",
        "limits",
    )
]
TIMELINE_BACKGROUND = {
    "tool": "toolu_tl_bg",
    "watch": False,
    "id": "claude:shell:btl5race1",
    "description": "Race tests",
    "output": "/tmp/claude-1000/-workspace/test-session/tasks/btl5race1.output",
}


def emit_timed_tool(tool_id: str, name: str, input_obj: dict, ms: int, source: dict | None) -> None:
    extra = {"source": source} if source else {}
    emit({"type": "tool_use", "id": tool_id, "name": name, "input": input_obj, **extra})
    advance_clock(ms)
    emit({"type": "tool_result", "tool_use_id": tool_id, "duration_ms": ms, **extra})


class TimelineCalls:
    """Emits per-call usage whose context grows as tool results join the prompt.

    The root caches its prompt prefix, so each call reads the previous context
    from cache and writes only what the last step added. A subagent starts
    from its own small prompt and reports no context window.
    """

    def __init__(self, model: str, context: int, window: int, source: dict | None) -> None:
        self.model = model
        self.context = 0
        self.pending = context
        self.window = window
        self.source = source
        self.count = 0
        self.totals: dict[str, int] = {}

    def call(self, added: int, output: int, reasoning: int) -> None:
        self.count += 1
        self.pending += added
        usage = {
            "input_tokens": 3 + self.count % 7,
            "cache_creation_input_tokens": self.pending,
            "cache_read_input_tokens": self.context,
            "output_tokens": output,
            "reasoning_output_tokens": reasoning,
        }
        for key, value in usage.items():
            self.totals[key] = self.totals.get(key, 0) + value
        record = {"type": "usage", "usage": usage, "model": self.model, "per_call": True}
        if self.window:
            record["context_window"] = self.window
        if self.source:
            record["source"] = self.source
        emit(record)
        self.context += self.pending + output
        self.pending = 0


def emit_turn_timeline_turn(turns: int) -> None:
    """Emit a dense turn spread over a minute of fixture time for the turn view.

    Many short reads crowd the Tools lane, two searches overlap, a subagent runs
    its own tools, an edit fails, and a background command outlives the result.
    Eleven tool names, three of them concurrent, fill the tool-time breakdown.
    Every API call reports its own usage. A harness-queued turn without a prompt
    reports the command's end.
    """
    started = last_timestamp_ms
    root = TimelineCalls("claude-opus-5-5", 14000, 200000, None)
    emit({"type": "thinking", "text": "Finding where requests enter the server before adding rate limits."})
    advance_clock(1200)
    root.call(0, 180, 120)
    emit_text("Reading the server package to find the request entry points.")
    for i, path in enumerate(TIMELINE_READS):
        advance_clock(300 + (i * 211) % 700)
        root.call(1200 + (i * 613) % 2400, 60 + (i * 37) % 90, 0)
        emit_timed_tool(f"toolu_tl_read_{i}", "Read", {"file_path": path}, 120 + (i * 97) % 400, None)
    advance_clock(900)
    root.call(2100, 240, 160)
    emit_tool_use("toolu_tl_grep", "Grep", {"pattern": "http.Handle", "path": "backend/internal/server"})
    emit_tool_use("toolu_tl_glob", "Glob", {"pattern": "backend/**/*_test.go"})
    advance_clock(700)
    emit_tool_result("toolu_tl_glob", 700)
    advance_clock(900)
    emit_tool_result("toolu_tl_grep", 1600)
    advance_clock(1500)
    root.call(5200, 420, 300)
    agent = {"tool": "toolu_tl_agent", "id": "claude:task:tl7review"}
    emit_tool_use(
        agent["tool"],
        "Agent",
        {"description": "Review rate limit tests", "prompt": "Find tests that exercise request limits."},
    )
    subagent = {
        "tool_use_id": agent["tool"],
        "id": agent["id"],
        "label": "Review rate limit tests",
        "prompt": "Find tests that exercise request limits.",
        "status": "running",
    }
    emit({"type": "native_subagent", "subagent": subagent})
    child = {"agentID": agent["id"]}
    helper = TimelineCalls("claude-haiku-5-5", 6000, 0, child)
    advance_clock(800)
    helper.call(0, 90, 0)
    emit({"type": "text", "text": "Searching the tests for request limits.", "source": child})
    for i, path in enumerate(("router_test.go", "middleware_test.go", "limits_test.go")):
        advance_clock(400)
        helper.call(1800, 70, 0)
        emit_timed_tool(f"toolu_tl_child_{i}", "Read", {"file_path": f"backend/internal/server/{path}"}, 300, child)
    advance_clock(600)
    helper.call(2200, 110, 0)
    emit_timed_tool(
        "toolu_tl_child_test", "Bash", {"command": "go test ./backend/internal/server -run Limit"}, 6400, child
    )
    advance_clock(900)
    helper.call(900, 160, 0)
    emit(
        {
            "type": "native_subagent",
            "subagent": {"id": agent["id"], "status": "completed", "result": "No test covers limits."},
        }
    )
    emit_tool_result(agent["tool"], 11000)
    advance_clock(2400)
    root.call(600, 520, 380)
    emit(
        {
            "type": "tool_use",
            "id": "toolu_tl_edit_1",
            "name": "Edit",
            "input": {"file_path": "backend/internal/server/router.go", "old_string": "mux :=", "new_string": "mux :="},
        }
    )
    advance_clock(200)
    emit({"type": "tool_result", "tool_use_id": "toolu_tl_edit_1", "error": "old_string not found"})
    advance_clock(1800)
    root.call(400, 300, 90)
    emit_timed_tool(
        "toolu_tl_edit_2",
        "Edit",
        {
            "file_path": "backend/internal/server/router.go",
            "old_string": "return mux",
            "new_string": "return limit(mux)",
        },
        250,
        None,
    )
    advance_clock(1500)
    root.call(300, 140, 0)
    emit_background_bash(TIMELINE_BACKGROUND)
    advance_clock(900)
    root.call(250, 90, 0)
    emit_timed_tool("toolu_tl_vet", "Bash", {"command": "go vet ./backend/..."}, 5200, None)
    advance_clock(300)
    root.call(1100, 260, 70)
    emit_timed_tool(
        "toolu_tl_todo", "TodoWrite", {"todos": [{"content": "Document the limiter", "status": "pending"}]}, 90, None
    )
    advance_clock(200)
    root.call(300, 210, 0)
    emit_tool_use("toolu_tl_search", "WebSearch", {"query": "token bucket rate limiter Go"})
    emit_tool_use("toolu_tl_fetch", "WebFetch", {"url": "https://pkg.go.dev/golang.org/x/time/rate", "prompt": "Burst"})
    emit_tool_use("toolu_tl_docs", "mcp__docs__search_documentation", {"query": "rate limit headers"})
    advance_clock(300)
    emit_tool_result("toolu_tl_docs", 300)
    advance_clock(250)
    emit_tool_result("toolu_tl_search", 550)
    advance_clock(250)
    emit_tool_result("toolu_tl_fetch", 800)
    advance_clock(200)
    root.call(6400, 640, 210)
    emit_timed_tool(
        "toolu_tl_write",
        "Write",
        {"file_path": "docs/rate_limits.md", "content": "# Rate limits\n"},
        110,
        None,
    )
    advance_clock(400)
    root.call(200, 90, 0)
    emit_text("Requests now pass through the limiter. The race tests run in the background.")
    # The result sums the calls, as Claude's does, and keeps the harness cost.
    emit(
        {
            "type": "result",
            "subtype": "success",
            "result": "Rate limits added",
            "num_turns": turns,
            "total_cost_usd": 0.01,
            "duration_ms": last_timestamp_ms - started,
            "duration_api_ms": 0,
            "context_window": 200000,
            "usage": {key: root.totals[key] + helper.totals[key] for key in root.totals},
        }
    )
    advance_clock(25000)
    emit_command_end(TIMELINE_BACKGROUND, f'Background command "{TIMELINE_BACKGROUND["description"]}" completed', 0)
    queued = last_timestamp_ms
    advance_clock(1500)
    emit_text("The race tests passed.")
    emit_result(turns, "Race tests passed", duration=last_timestamp_ms - queued)


def emit_native_subagents_turn(turns: int) -> None:
    """Emit canonical concurrent activity; this is UI coverage, not harness evidence."""
    # Parent narration before, between, and after the runs gives the transcript
    # the content boundaries the UI anchors each settled card to.
    emit_text("Delegating the review to two parallel agents and a chained batch.")
    visual = os.environ.get("CAIC_E2E_VISUALS") == "1"
    for identity in ("joker", "reviewer"):
        emit(
            {
                "type": "native_subagent",
                "subagent": {
                    "id": {"joker": "token-validation", "reviewer": "session-migration"}[identity]
                    if visual
                    else identity,
                    "group_id": "parallel",
                    "label": ("Token validation" if identity == "joker" else "Session migration")
                    if visual
                    else identity,
                    "prompt": "Review authentication boundaries and regression coverage"
                    if visual
                    else "Tell a README joke",
                    "tool_use_id": f"fake-native-{identity}",
                    "status": "running",
                },
            }
        )
    for identity, status, result in (
        (
            "joker",
            "completed",
            "Confirmed token expiry validation and added a regression test."
            if visual
            else "Read me before you judge me.",
        ),
        ("reviewer", "failed", "Permission denied"),
    ):
        emit(
            {
                "type": "native_subagent",
                "subagent": {
                    "id": {"joker": "token-validation", "reviewer": "session-migration"}[identity]
                    if visual
                    else identity,
                    "status": status,
                    "result": result,
                },
            }
        )
    emit_text("Both reviews are in; the batch is still running.")
    emit(
        {
            "type": "native_subagent",
            "subagent": {
                "id": "batch",
                "scope": "batch",
                "label": "Cross-service integration" if visual else "Chained work",
                "status": "unknown",
            },
        }
    )
    # A paused resumable run is neither active nor an interruption.
    emit(
        {
            "type": "native_subagent",
            "subagent": {
                "id": "paused-batch",
                "scope": "batch",
                "label": "Browser regression suite" if visual else "Detached workflow",
                "status": "running",
            },
        }
    )
    emit(
        {
            "type": "native_subagent",
            "subagent": {
                "id": "paused-batch",
                "status": "paused",
            },
        }
    )
    emit_result(turns, "Native activity finished")


def gate(command: str) -> bool:
    line = sys.stdin.readline()
    if not line or "\x00" in line:
        return False
    if json.loads(line)["text"] != command:
        raise ValueError(f"Expected fake-agent command {command!r}, got {line!r}")
    return True


def emit_live_turn(turns: int) -> None:
    """Emit a turn whose tools arrive across input gates, so an open turn view grows.

    A read starts at once. FAKE_LIVE_CONTINUE ends it and starts a test run;
    FAKE_LIVE_FINISH ends the run and the turn.
    """
    emit_text("Reading the server entry point.")
    emit_tool_use("toolu_live_read", "Read", {"file_path": "backend/cmd/caic/main.go"})
    if not gate("FAKE_LIVE_CONTINUE"):
        return
    emit_tool_result("toolu_live_read", 400)
    emit_text("Running the server tests.")
    emit_tool_use("toolu_live_test", "Bash", {"command": "go test ./backend/..."})
    if not gate("FAKE_LIVE_FINISH"):
        return
    emit_tool_result("toolu_live_test", 1200)
    emit_text("The server tests pass.")
    emit_result(turns, "Live turn complete")


def emit_concurrent_streams_turn(turns: int) -> None:
    """Interleave independent items, advancing only after real user-input gates."""

    def record(payload: dict, thread: str, item: str) -> None:
        payload["source"] = {
            "threadID": thread,
            "turnID": "review-turn",
            "itemID": item,
            "rootThreadID": "parent",
            **({"agentID": f"codex:thread:{thread}"} if thread != "parent" else {}),
        }
        emit(payload)

    def message(kind: str, thread: str, item: str, text: str) -> None:
        record({"type": kind, "text": text}, thread, item)

    for thread, label in (("child-a", "Polygon research"), ("child-b", "Assembly research")):
        emit(
            {
                "type": "native_subagent",
                "subagent": {"id": f"codex:thread:{thread}", "label": label, "status": "running", "background": True},
            }
        )
    record(
        {"type": "usage", "usage": {"input_tokens": 100}, "model": "root-model", "context_window": 200000},
        "parent",
        "usage",
    )
    for thread in ("parent", "child-a", "child-b"):
        record(
            {"type": "tool_use", "id": "shared-tool", "name": "Bash", "input": {"command": f"echo {thread}"}},
            thread,
            "command",
        )
        record(
            {"type": "tool_output_delta", "tool_use_id": "shared-tool", "delta": f"{thread} output start\n"},
            thread,
            "command",
        )
    record(
        {"type": "usage", "usage": {"input_tokens": 999}, "model": "child-model", "context_window": 1024},
        "child-a",
        "usage",
    )
    message("thinking_delta", "parent", "reasoning", "Considering ")
    message("text_delta", "child-a", "polygon", "- A bow")
    message("text_delta", "child-b", "assembly", "Assembly draft")
    message("text_delta", "child-a", "discarded", "Discarded draft")
    if not gate("FAKE_CONCURRENT_STREAMS_CONTINUE"):
        return
    message("text_delta", "child-a", "polygon", "tie uses `Ear")
    message("thinking_delta", "parent", "reasoning", "validation while children research.")
    message("text_delta", "child-b", "assembly", " remains in flight.")
    message("text_delta", "child-a", "polygon", "cut`.")
    message("text", "child-a", "polygon", "- A bowtie uses `Earcut`.\n- Validate contours before triangulation.")
    message("text", "child-a", "discarded", "")
    message("thinking", "parent", "reasoning", "Considering validation while children research.")
    message("text", "parent", "summary", "Parent summary remains separate.")
    # Thousands of fragments exercise bounded per-invocation output snapshots.
    for _ in range(1000):
        record({"type": "tool_output_delta", "tool_use_id": "shared-tool", "delta": "."}, "child-a", "command")
    record(
        {"type": "tool_output_delta", "tool_use_id": "shared-tool", "delta": "\nchild-a output complete"},
        "child-a",
        "command",
    )
    if not gate("FAKE_CONCURRENT_STREAMS_RECONNECT"):
        return
    for thread in ("child-a", "child-b", "parent"):
        record(
            {"type": "tool_output_delta", "tool_use_id": "shared-tool", "delta": f"\n{thread} missed output"},
            thread,
            "command",
        )
    message("thinking", "parent", "reasoning", "Considering validation while children research.")
    if not gate("FAKE_CONCURRENT_STREAMS_A_COMPLETE"):
        return
    record({"type": "tool_result", "tool_use_id": "shared-tool", "duration_ms": 50}, "child-a", "command")
    if not gate("FAKE_CONCURRENT_STREAMS_FINISH"):
        return
    assembly = "Assembly frames preserve local transforms."
    message("text", "child-b", "assembly", assembly)
    record(
        {"type": "tool_result", "tool_use_id": "shared-tool", "duration_ms": 70, "error": "Assembly command failed"},
        "child-b",
        "command",
    )
    record({"type": "system", "subtype": "error", "detail": "Assembly child failed independently"}, "child-b", "error")
    record(
        {"type": "tool_output_delta", "tool_use_id": "orphan-output", "delta": "Unmatched output remains visible"},
        "child-b",
        "orphan-output",
    )
    record(
        {"type": "tool_result", "tool_use_id": "orphan-result", "error": "Unmatched result remains visible"},
        "child-a",
        "orphan-result",
    )
    for thread, status, result in (
        ("child-a", "completed", "Polygon review finished; evidence preserved."),
        ("child-b", "failed", "Assembly child failed independently"),
    ):
        emit(
            {
                "type": "native_subagent",
                "subagent": {"id": f"codex:thread:{thread}", "status": status, "result": result},
            }
        )
    if not gate("FAKE_CONCURRENT_STREAMS_ROOT_FINISH"):
        return
    record(
        {"type": "tool_output_delta", "tool_use_id": "shared-tool", "delta": "parent output complete"},
        "parent",
        "command",
    )
    record({"type": "tool_result", "tool_use_id": "shared-tool", "duration_ms": 90}, "parent", "command")
    record(
        {
            "type": "result",
            "subtype": "success",
            "result": "Concurrent review finished",
            "num_turns": turns,
            "total_cost_usd": 0.37,
            "duration_ms": 500,
            "context_window": 200000,
            "usage": {
                "input_tokens": 123,
                "output_tokens": 45,
                "cache_creation_input_tokens": 6,
                "cache_read_input_tokens": 78,
            },
        },
        "parent",
        "result",
    )


def main() -> None:
    version = "fake"
    if os.environ.get("CAIC_E2E_VISUALS") == "1":
        # Other harnesses do not report a Claude Code release number.
        version = "2.1.185" if os.environ.get("CAIC_FAKE_HARNESS") == "claude" else ""
    emit(
        {
            "type": "init",
            "session_id": "0ac768bc-a58b-4f81-9e49-c0147d33d640"
            if os.environ.get("CAIC_E2E_VISUALS") == "1"
            else "test-session",
            "cwd": "/workspace",
            "model": os.environ.get("CAIC_FAKE_MODEL") or "fake-model",
            "reported_effort": os.environ.get("CAIC_FAKE_EFFORT", ""),
            "claude_code_version": version,
        }
    )

    turns = 0
    for line in sys.stdin:
        # The server sends a null byte as the stop sentinel (see relay
        # shutdown protocol in agent.go). Without a relay, the sentinel
        # arrives on our stdin directly — exit cleanly.
        if "\x00" in line:
            break
        line = json.loads(line)["text"]
        if not line:
            continue
        turns += 1
        emit({"type": "turn_started"})

        # Exact keyword triggers (for e2e tests).
        if line.startswith("FAKE_CONCURRENT_STREAMS"):
            emit_concurrent_streams_turn(turns)
            continue
        if line.startswith("FAKE_NATIVE_SKILL"):
            # Canonical DSH tool projection, not native protocol evidence.
            emit({"type": "skill_read", "id": "native-skill", "skill": "code-quality"})
            emit({"type": "skill_load", "skill": "code-quality", "source_tool_use_id": "native-skill"})
            emit({"type": "tool_use", "id": "native-skill", "name": "skill", "input": {"name": "code-quality"}})
            emit({"type": "tool_output_delta", "tool_use_id": "native-skill", "delta": "Native skill contents"})
            emit({"type": "tool_result", "tool_use_id": "native-skill"})
            emit(
                {
                    "type": "native_subagent",
                    "subagent": {
                        "id": "dsh-child",
                        "tool_use_id": "native-subagent",
                        "label": "Review skill integration",
                        "prompt": "Inspect code-quality integration",
                        "status": "unknown",
                        "background": True,
                    },
                }
            )
            emit_text("Native skill loaded.")
            emit_result(turns, "Native skill complete")
            continue
        if line.startswith("FAKE_SKILLS"):
            if "compact" in line:
                emit({"type": "system", "subtype": "compact_boundary"})
                emit_text("Context compacted; skills must be loaded again.")
            else:
                emit(
                    {
                        "type": "skill_load",
                        "skill": "code-quality",
                        "path": "/home/user/.agents/skills/code-quality/SKILL.md",
                    }
                )
                emit({"type": "skill_load", "skill": "review", "path": "/home/user/.agents/skills/review/SKILL.md"})
                emit_text("Loaded code-quality and review into this context.")
            emit_result(turns, "Skill context updated")
            continue
        if line.startswith("FAKE_LIVE_TURN"):
            emit_live_turn(turns)
            continue
        if line.startswith("FAKE_TURN_TIMELINE"):
            emit_turn_timeline_turn(turns)
            continue
        if line.startswith("FAKE_NATIVE_SUBAGENTS"):
            emit_native_subagents_turn(turns)
            continue
        if line.startswith("FAKE_BACKGROUND_WATCH_ONLY"):
            emit_background_watch_only_turn(turns)
            continue
        if line.startswith("FAKE_BACKGROUND_EDGE_AGENT_DONE"):
            emit_background_edge_agent_done_turn(turns)
            continue
        if line.startswith("FAKE_BACKGROUND_EDGES"):
            emit_background_edges_turn(turns)
            continue
        if line.startswith("FAKE_BACKGROUND_WORK"):
            emit_background_turn(turns, True)
            continue
        if line.startswith("FAKE_COMPACT"):
            # The harness compacts before answering, as on reaching its threshold.
            emit(
                {
                    "type": "system",
                    "subtype": "compact_boundary",
                    "context_tokens_before": 150000,
                    "context_tokens_after": 20000,
                }
            )
            emit({"type": "usage", "usage": {}, "context_used": 26000, "context_window": 200000})
            emit_text("Context compacted; continuing from the summary.")
            emit_result(turns, "Compaction complete")
            continue
        if line.startswith("FAKE_CONTEXT_OCCUPANCY"):
            if " RESET" in line:
                emit({"type": "system", "subtype": "context_cleared"})
            emit({"type": "usage", "usage": {"input_tokens": 123, "cache_read_input_tokens": 456}})
            if " RESET" not in line:
                emit(
                    {
                        "type": "usage",
                        "usage": {},
                        "context_used": 0 if " ZERO" in line else 45000,
                        "context_window": 200000,
                    }
                )
            emit_text("Native context occupancy reported")
            emit_result(turns, "Context occupancy complete")
            continue
        if line == "FAKE_LIFECYCLE" or line.startswith("FAKE_LIFECYCLE "):
            emit_lifecycle_turn(turns)
            continue
        if line == "FAKE_QUOTA_RECOVERY" or line.startswith("FAKE_QUOTA_RECOVERY "):
            emit_quota_recovery_turn(turns)
            continue
        if "FAKE_PLAN" in line:
            emit_plan_turn(turns)
            continue
        if "FAKE_ASK" in line:
            emit_ask_turn(turns)
            continue
        if "FAKE_DEMO" in line:
            emit_demo_turn(turns, "FAKE_DEMO_GATED" in line)
            continue

        # Natural prompt detection (for screenshots with clean prompts).
        lower = line.lower()
        if os.environ.get("CAIC_E2E_VISUALS") == "1" and lower == "implement rate limiting for api endpoints":
            emit_capture_running()
            break
        if NATURAL_NATIVE_RE.search(lower):
            emit_native_subagents_turn(turns)
            continue
        if NATURAL_BACKGROUND_RE.search(lower):
            emit_background_turn(turns, False)
            continue
        if NATURAL_PLAN_RE.search(lower):
            emit_plan_turn(turns)
            continue
        if NATURAL_ASK_RE.search(lower):
            emit_ask_turn(turns)
            continue
        if NATURAL_DEMO_RE.search(lower):
            emit_demo_turn(turns, False)
            continue

        if "FAKE_WIDGET" in line or (os.environ.get("CAIC_E2E_VISUALS") == "1" and "refraction" in lower):
            emit_widget_turn(turns)
            continue

        joke = JOKES[(turns - 1) % len(JOKES)]
        emit_text(joke)
        emit_result(turns, joke)


if __name__ == "__main__":
    main()
