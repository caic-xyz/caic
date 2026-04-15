# Fake Agent for E2E Testing

Test harness simulating a Claude Code agent for e2e testing. Build-gated
behind `//go:build e2e` — excluded from production binaries.

## Architecture

- `fake.go` — `agent.Backend` implementation: spawns the Python script, owns the wire format
- `embed.go` — Embeds `fake_agent.py` into the Go binary (e2e build tag only)
- `fake_agent.py` — Python script emitting Claude Code NDJSON wire format

`fake.Backend` implements `agent.WireFormat` directly: prompts are written
as plain text (the Python script reads lines and matches keywords), output
is parsed by `parse.go` — a flat NDJSON parser with no claudecode dependency.
Each JSON line's `type` field maps directly to an `agent.Message` type.

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
