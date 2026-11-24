// Shares conversation:// link resolution with rendered Markdown without pulling Markdown into the eager app shell.

import { createContext } from "solid-js";

/** Resolves a `conversation://<id>` destination to an in-app route, or null when unresolvable. */
export const MarkdownConversationContext =
  createContext<(conversationId: string, taskId: string | null) => string | null>();
