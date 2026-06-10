// MCP JSON-RPC client for listing backend tool descriptors and dispatching tool calls via the MCP endpoint.

const MCP_PROTOCOL_VERSION = "2026-07-28";

function mcpMeta() {
  return {
    "io.modelcontextprotocol/protocolVersion": MCP_PROTOCOL_VERSION,
    "io.modelcontextprotocol/clientInfo": {
      name: "caic-frontend",
      version: "1.0.0",
    },
    "io.modelcontextprotocol/clientCapabilities": {},
  };
}

let _idCounter = 0;

async function mcpRequest(
  method: string,
  params: Record<string, unknown>,
  name?: string,
): Promise<unknown> {
  const id = ++_idCounter;
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "Mcp-Protocol-Version": MCP_PROTOCOL_VERSION,
    "Mcp-Method": method,
  };
  if (name !== undefined) headers["Mcp-Name"] = name;

  const resp = await fetch("/api/caic/v1/mcp", {
    method: "POST",
    headers,
    body: JSON.stringify({
      jsonrpc: "2.0",
      id,
      method,
      params: { ...params, _meta: mcpMeta() },
    }),
  });
  const rpc = (await resp.json()) as {
    result?: unknown;
    error?: { code: number; message: string };
  };
  if (rpc.error) throw new Error(rpc.error.message);
  return rpc.result;
}

export interface McpToolDescriptor {
  name: string;
  title?: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

export async function mcpListTools(): Promise<McpToolDescriptor[]> {
  const result = (await mcpRequest("tools/list", {})) as {
    tools: McpToolDescriptor[];
  };
  return result.tools;
}

export interface McpToolResult {
  structuredContent: Record<string, unknown>;
  isError?: boolean;
}

export async function mcpCallTool(
  name: string,
  args: Record<string, unknown>,
): Promise<McpToolResult> {
  return (await mcpRequest(
    "tools/call",
    { name, arguments: args },
    name,
  )) as McpToolResult;
}
