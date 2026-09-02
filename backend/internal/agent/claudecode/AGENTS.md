# Claude Code Agent Backend

Implements `agent.Backend` for Claude Code. Manages the widget plugin
(`widget-plugin/`) deployed to containers via `embed.FS`.

## References

Source code:
- https://github.com/anthropics/claude-code

Claude Code headless:
- https://code.claude.com/docs/en/headless: headless mode overview
- https://code.claude.com/docs/en/prompt-caching: Claude Code main-conversation, subagent, authentication, and override TTL behavior
- https://platform.claude.com/docs/en/agent-sdk/streaming-output: streaming protocol wire format
- https://platform.claude.com/docs/en/build-with-claude/prompt-caching: cache TTLs and usage-field semantics
- git clone https://github.com/anthropics/claude-agent-sdk-python for SDK types (`src/claude_agent_sdk/types.py`)
- git clone https://github.com/anthropics/claude-agent-sdk-python to get the SDK and understand types, in particular `src/claude_agent_sdk/types.py`

Claude Code plugins:
- https://code.claude.com/docs/en/plugins: plugin creation, `--plugin-dir`, plugin structure overview
- https://code.claude.com/docs/en/plugins-reference: full schema for plugin.json, MCP/LSP/hooks config, debugging
- https://code.claude.com/docs/en/mcp: MCP server configuration, plugin MCP servers, `${CLAUDE_PLUGIN_ROOT}` variable
- https://code.claude.com/docs/en/skills: skill authoring (SKILL.md format, frontmatter, progressive disclosure)

## Prompt Cache Controls

Claude Code divides requests into a main-conversation bucket and an auxiliary
bucket that includes subagents, forks, compaction, titles, and other helper
requests. Subscription main conversations default to a one-hour cache. The
auxiliary bucket defaults to five minutes, except that server-controlled helper
requests use one hour on a subscription. API-key, cloud-provider, and
usage-credit requests default to five minutes in both buckets. A named subagent
starts a separate cache prefix; a fork can read the parent's existing prefix on
its first request, but the fork's cache writes use the auxiliary bucket.

Claude Code 2.1.242 and later accepts only `5m` or `1h` for the direct TTL
controls. Their documented precedence is:

1. `FORCE_PROMPT_CACHING_5M=1` forces both buckets to five minutes.
2. `CLAUDE_CODE_PROMPT_CACHE_TTL` controls the main bucket, and
   `CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL` controls the auxiliary bucket.
3. The corresponding settings are `promptCacheTtl` and
   `subagentPromptCacheTtl`.
4. A subagent's `experimental.cacheTtl` frontmatter applies next on Claude Code
   2.1.248 and later. A `1h` value there is ignored while a subscription is
   using usage credits.
5. `ENABLE_PROMPT_CACHING_1H=1` selects one hour for both buckets.
6. Otherwise Claude Code applies the defaults above.

The older `ENABLE_PROMPT_CACHING_1H_BEDROCK` variable is deprecated but remains
supported according to the Claude Code changelog.

`DISABLE_PROMPT_CACHING` disables prompt caching globally. The model-specific
variants are `DISABLE_PROMPT_CACHING_HAIKU`,
`DISABLE_PROMPT_CACHING_SONNET`, `DISABLE_PROMPT_CACHING_OPUS`, and
`DISABLE_PROMPT_CACHING_FABLE`.

Authentication and provider selection can indirectly change the default TTL.
In particular, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` selects non-plan
authentication, while `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, and
`CLAUDE_CODE_USE_FOUNDRY` select cloud providers. caic removes
`ANTHROPIC_API_KEY` when it supplies Claude subscription OAuth credentials; see
`claude.go`. `ANTHROPIC_BASE_URL` and `ANTHROPIC_BEDROCK_BASE_URL` are routing
controls, not TTL controls. A configured gateway can nevertheless affect
caching by forwarding, rejecting, or removing `cache_control` fields.

Do not infer an applied TTL from any setting, environment variable,
authentication method, or agent role. Populate `agent.Usage.CacheTTLSeconds`
only from the response's `cache_creation.ephemeral_5m_input_tokens` and
`cache_creation.ephemeral_1h_input_tokens` fields.

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
