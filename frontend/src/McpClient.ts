// MCP JSON-RPC client for listing backend tool descriptors and dispatching tool calls via the MCP endpoint.

import { createApiClient as createMcpApiClient } from "@mcp-sdk/api.gen";

const MCP_PROTOCOL_VERSION = "2026-07-28";
const mcpApi = createMcpApiClient((path, init) =>
  fetch(`/api/caic/v1/mcp${path}`, init),
);

type JsonObject = Record<string, unknown>;

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
let _toolCache = new Map<string, McpToolDescriptor>();

async function mcpRequest(
  method: string,
  params: JsonObject,
  opts: { name?: string; paramHeaders?: Record<string, string> } = {},
): Promise<unknown> {
  const id = ++_idCounter;
  const headers: Record<string, string> = {
    Accept: "application/json, text/event-stream",
    "Content-Type": "application/json",
    "Mcp-Protocol-Version": MCP_PROTOCOL_VERSION,
    "Mcp-Method": method,
    ...opts.paramHeaders,
  };
  if (opts.name !== undefined) headers["Mcp-Name"] = opts.name;

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

export interface McpToolAnnotations {
  title?: string;
  readOnlyHint?: boolean;
  destructiveHint?: boolean;
  idempotentHint?: boolean;
  openWorldHint?: boolean;
}

export interface McpToolDescriptor {
  name: string;
  title?: string;
  description: string;
  inputSchema: JsonObject;
  outputSchema?: JsonObject;
  annotations?: McpToolAnnotations;
}

export async function mcpServerInstructions(): Promise<string> {
  return mcpApi.serverInstructions();
}

export async function mcpListTools(): Promise<McpToolDescriptor[]> {
  const tools: McpToolDescriptor[] = [];
  let cursor: string | undefined;
  do {
    const result = (await mcpRequest(
      "tools/list",
      cursor === undefined ? {} : { cursor },
    )) as {
      tools: McpToolDescriptor[];
      nextCursor?: string;
    };
    tools.push(...result.tools);
    cursor = result.nextCursor;
  } while (cursor !== undefined && cursor !== "");

  _toolCache = new Map(tools.map((tool) => [tool.name, tool]));
  return tools;
}

export interface McpToolResult {
  structuredContent: JsonObject;
  isError?: boolean;
}

export async function mcpCallTool(
  name: string,
  args: JsonObject,
): Promise<McpToolResult> {
  const result = (await mcpRequest(
    "tools/call",
    { name, arguments: args },
    { name, paramHeaders: mcpParamHeaders(_toolCache.get(name), args) },
  )) as {
    structuredContent?: JsonObject;
    content?: Array<{ type?: string; text?: string }>;
    isError?: boolean;
  };
  return {
    structuredContent:
      result.structuredContent ?? textContentAsStructuredError(result.content),
    isError: result.isError,
  };
}

function textContentAsStructuredError(
  content: Array<{ type?: string; text?: string }> | undefined,
): JsonObject {
  const text = content?.find((block) => block.type === "text")?.text;
  return text === undefined ? {} : { error: text };
}

function mcpParamHeaders(
  tool: McpToolDescriptor | undefined,
  args: JsonObject,
): Record<string, string> {
  if (tool === undefined) return {};
  const headers: Record<string, string> = {};
  collectMcpParamHeaders(tool.inputSchema, args, headers);
  return headers;
}

function collectMcpParamHeaders(
  schema: JsonObject,
  args: unknown,
  headers: Record<string, string>,
): void {
  const headerName = schema["x-mcp-header"];
  if (typeof headerName === "string" && args !== null && args !== undefined) {
    headers[`Mcp-Param-${headerName}`] = encodeMcpHeaderValue(String(args));
  }

  const properties = schema.properties;
  if (!isJsonObject(properties) || !isJsonObject(args)) return;
  for (const [key, child] of Object.entries(properties)) {
    if (!isJsonObject(child)) continue;
    collectMcpParamHeaders(child, args[key], headers);
  }
}

function encodeMcpHeaderValue(value: string): string {
  if (isPlainMcpHeaderValue(value)) return value;
  const bytes = new TextEncoder().encode(value);
  const binary = Array.from(bytes, (byte) => String.fromCharCode(byte)).join(
    "",
  );
  return `=?base64?${btoa(binary)}?=`;
}

function isPlainMcpHeaderValue(value: string): boolean {
  if (value.startsWith("=?base64?") && value.endsWith("?=")) return false;
  if (value.trim() !== value) return false;
  return /^[\x20\x21-\x7e]*$/.test(value);
}

function isJsonObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
