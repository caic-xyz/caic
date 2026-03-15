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

        joke = JOKES[(turns - 1) % len(JOKES)]
        emit_text(joke)
        emit_result(turns, joke)


if __name__ == "__main__":
    main()
