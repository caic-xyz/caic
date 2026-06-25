// Golden tests for generated voice gateway protocol DTO JSON.

import { describe, expect, it } from "vitest";

import {
  type ContextUpdate,
  type UserMessage,
  type SessionSetup,
  type ToolResult,
  MessageKindContextUpdate,
  MessageKindUserMessage,
  MessageKindSessionSetup,
  MessageKindToolResult,
} from "@voicegateway-sdk/types.gen";

describe("voice gateway generated protocol DTOs", () => {
  it("serializes session setup exactly", () => {
    const msg: SessionSetup = {
      kind: MessageKindSessionSetup,
      voice: { name: "Kore", language: "en" },
      tools: [
        {
          name: "tasks_list",
          description: "List tasks",
          parameters: { type: "object", properties: {} },
        },
      ],
      context: { systemInstruction: "system prompt" },
    };

    expect(JSON.stringify(msg)).toBe(
      '{"kind":"session.setup","voice":{"name":"Kore","language":"en"},"tools":[{"name":"tasks_list","description":"List tasks","parameters":{"type":"object","properties":{}}}],"context":{"systemInstruction":"system prompt"}}',
    );
  });

  it("serializes context updates exactly", () => {
    const msg: ContextUpdate = {
      kind: MessageKindContextUpdate,
      context: { text: "status update" },
    };

    expect(JSON.stringify(msg)).toBe(
      '{"kind":"context.update","context":{"text":"status update"}}',
    );
  });

  it("serializes user messages exactly", () => {
    const msg: UserMessage = {
      kind: MessageKindUserMessage,
      text: "Say exactly one word: Ready",
    };

    expect(JSON.stringify(msg)).toBe(
      '{"kind":"user.message","text":"Say exactly one word: Ready"}',
    );
  });

  it("serializes tool results exactly", () => {
    const msg: ToolResult = {
      kind: MessageKindToolResult,
      id: "call-1",
      name: "tasks_list",
      result: { ok: true },
    };

    expect(JSON.stringify(msg)).toBe(
      '{"kind":"tool.result","id":"call-1","name":"tasks_list","result":{"ok":true}}',
    );
  });
});
