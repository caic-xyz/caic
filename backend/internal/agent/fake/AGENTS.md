# Fake Agent Package

Test harness simulating a Claude Code agent for e2e testing. Build-gated
behind `//go:build e2e` — excluded from production binaries.

## Architecture

- `embed.go` — Embeds `fake_agent.py` into the Go binary (e2e build tag only)
- `fake_agent.py` — Python script emitting Claude Code NDJSON wire format

The Go side (`cmd/caic/fake_enabled.go`) spawns the Python script as a
subprocess. Responses are parsed by `claude.ParseMessage()` — same parser
as the real Claude backend — so the full message pipeline is exercised.

## Magic Keywords

Prompts containing these keywords trigger specific behaviors:

| Keyword | Behavior |
|---------|----------|
| `FAKE_PLAN` | 5-step auth fix plan (ExitPlanMode flow) |
| `FAKE_ASK` | Multi-choice AskUserQuestion card |
| `FAKE_DEMO` | Realistic multi-step scenario (cycles through 3) |
| `FAKE_WIDGET` | Interactive Snell's Law SVG widget |

Natural language fallback: "plan"/"design" → plan, "which"/"choose" → ask,
"fix"/"add"/"implement" → demo, otherwise → cycling programmer jokes.

## Usage

```bash
make frontend-e2e   # Builds with -tags e2e, runs Playwright against fake backend
```
