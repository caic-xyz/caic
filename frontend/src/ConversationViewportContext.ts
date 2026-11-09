// Shares the shell-owned mobile conversation scroll viewport with routed task history.
import { createContext, type Accessor } from "solid-js";

export const ConversationViewportContext = createContext<Accessor<HTMLElement | undefined>>(() => undefined);
