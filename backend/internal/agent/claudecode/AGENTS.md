# Claude Code Package

Implements `agent.Backend` for Claude Code. Manages the widget plugin
(`widget-plugin/`) deployed to containers via `embed.FS`.

## References

Source code:
- https://github.com/anthropics/claude-code

Claude Code headless:
- https://code.claude.com/docs/en/headless: headless mode overview
- https://platform.claude.com/docs/en/agent-sdk/streaming-output: streaming protocol wire format
- git clone https://github.com/anthropics/claude-agent-sdk-python for SDK types (`src/claude_agent_sdk/types.py`)

Claude Code plugins:
- https://code.claude.com/docs/en/plugins: plugin creation, `--plugin-dir`, plugin structure overview
- https://code.claude.com/docs/en/plugins-reference: full schema for plugin.json, MCP/LSP/hooks config, debugging
- https://code.claude.com/docs/en/mcp: MCP server configuration, plugin MCP servers, `${CLAUDE_PLUGIN_ROOT}` variable
- https://code.claude.com/docs/en/skills: skill authoring (SKILL.md format, frontmatter, progressive disclosure)

## Widget Plugin

The `widget-plugin/` directory is a Claude Code plugin providing the
`show_widget` MCP tool and the widget design skill.

Key rules from the official docs:
- **Use `${CLAUDE_PLUGIN_ROOT}`** for all paths in `.mcp.json` and hooks.
  Hardcoded paths cause "MCP server fails" (documented common issue).
- `.mcp.json` at plugin root uses flat format (no `mcpServers` wrapper).
- `plugin.json` goes in `.claude-plugin/`; all other dirs (`skills/`, `commands/`, `agents/`) at plugin root.
- Plugin MCP tool naming: `mcp__plugin_<plugin-name>_<server-name>__<tool-name>`.
- **MCP stdio transport uses NDJSON** (newline-delimited JSON), NOT Content-Length
  framing. The server must read lines from stdin and write JSON lines to stdout.
- Haiku does not support Tool Search (`tool_reference` blocks). If tool search
  auto-enables (MCP tools >10% context), Haiku cannot discover deferred tools.
