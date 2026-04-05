#!/usr/bin/env python3
# Stdio MCP server exposing the show_widget tool.
#
# Implements the minimal MCP stdio protocol (JSON-RPC 2.0 over
# newline-delimited JSON on stdin/stdout). The tool itself is a
# no-op — rendering is handled by the streaming parser (WidgetTracker).

import json
import sys

SHOW_WIDGET_TOOL = {
    "name": "show_widget",
    "description": (
        "Render an interactive HTML widget inline in the conversation. "
        "Structure code so useful content appears early: <style> (short) "
        "\u2192 content HTML \u2192 <script> last. Scripts execute only after "
        "streaming completes."
    ),
    "inputSchema": {
        "type": "object",
        "properties": {
            "title": {
                "type": "string",
                "description": (
                    "Short descriptive title for the widget (e.g. 'compound_interest_calculator'). Use snake_case."
                ),
            },
            "widget_code": {
                "type": "string",
                "description": (
                    "Raw HTML fragment to render. No <!DOCTYPE>, <html>, "
                    "<head>, or <body> tags. May include <style> and "
                    "<script> tags. Scripts can load libraries from CDN "
                    "allowlist: cdnjs.cloudflare.com, cdn.jsdelivr.net, "
                    "unpkg.com, esm.sh."
                ),
            },
        },
        "required": ["title", "widget_code"],
    },
}


def read_message():
    """Read a JSON-RPC message from stdin (newline-delimited JSON)."""
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        return json.loads(line)
    return None


def write_message(msg):
    """Write a JSON-RPC message to stdout (newline-delimited JSON)."""
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def main():
    while True:
        msg = read_message()
        if msg is None:
            break
        method = msg.get("method")
        msg_id = msg.get("id")
        if method == "initialize":
            write_message(
                {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "result": {
                        "protocolVersion": "2025-11-25",
                        "capabilities": {"tools": {}},
                        "serverInfo": {"name": "caic-widget", "version": "1.0.0"},
                    },
                }
            )
        elif method == "notifications/initialized":
            pass  # No response for notifications.
        elif method == "tools/list":
            write_message(
                {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "result": {"tools": [SHOW_WIDGET_TOOL]},
                }
            )
        elif method == "tools/call":
            ok = "Widget rendered successfully. The user can see it."
            write_message(
                {
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "result": {"content": [{"type": "text", "text": ok}]},
                }
            )


if __name__ == "__main__":
    main()
