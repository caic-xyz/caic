# Gemini CLI Package

Implements `agent.Backend` for Google's Gemini CLI.
Translates Gemini's stream-json NDJSON protocol into normalized `agent.Message` types.

## Protocol

Gemini CLI runs with `--output-format stream-json` and `--yolo` (auto-approve).
Prompts sent as plain text on stdin; images not supported.

**Record types**: `init`, `message`, `tool_use`, `tool_result`, `result`.

## Architecture

- `gemini.go` — Backend lifecycle, plain-text prompt writer
- `wire.go` — Type probe for lazy JSON unmarshaling
- `record.go` — Typed record structs (`InitRecord`, `MessageRecord`, etc.)
- `parse.go` — Stateless parser: Gemini records → `agent.Message`

## Tool Name Normalization

Gemini uses lowercase tool names; `toolNameMap` normalizes to caic canonical names:

```
read_file, read_many_files → Read
write_file                 → Write
replace                    → Edit
run_shell_command          → Bash
grep, grep_search          → Grep
glob                       → Glob
web_fetch                  → WebFetch
google_web_search          → WebSearch
ask_user                   → AskUserQuestion
write_todos                → TodoWrite
list_directory             → ListDirectory
```

## References

Source code:
- https://github.com/google-gemini/gemini-cli

Documentation:
- https://geminicli.com/docs/: primary docs
- https://developers.google.com/gemini-code-assist/docs/gemini-cli: Google Developers docs

## Key Design Decisions

- **1M context window**: Gemini's large context reflected in `ContextWindowLimit`.
- **Forward compatibility**: all record types use `jsonutil.Overflow` for unknown fields.
- **Special tool dispatch**: `AskUserQuestion` → `AskMessage`, `TodoWrite` → `TodoMessage`.
