// Go Mode host-mode context: exposes native bridge capabilities, including voice state, to the hosted frontend.

import { createContext, createSignal, onCleanup, onMount, useContext, type Accessor, type JSX } from "solid-js";
import { useLocation, type SearchParams } from "@solidjs/router";

declare global {
  interface Window {
    goModeHost?: unknown;
  }
}

const HOST_MODE_PARAM = "goModeHost";

interface HostMode {
  isGoModeHost: Accessor<boolean>;
  browserNotificationsEnabled: Accessor<boolean>;
  browserVoiceEnabled: Accessor<boolean>;
  nativeVoiceConnected: Accessor<boolean>;
}

const HostModeContext = createContext<HostMode>();

function hasNativeHostMarker(): boolean {
  return window.goModeHost !== undefined;
}

export function hasHostModeQuery(query: SearchParams): boolean {
  const value = query[HOST_MODE_PARAM];
  return Array.isArray(value) ? value.includes("1") : value === "1";
}

/** Reads the native shell's voice-session state from its narrow WebView bridge. */
export function nativeVoiceConnected(host: unknown): boolean {
  if (typeof host !== "object" || host === null) return false;
  const readVoiceConnected = (host as { isVoiceConnected?: unknown }).isVoiceConnected;
  return typeof readVoiceConnected === "function" && readVoiceConnected.call(host) === true;
}

export function HostModeProvider(props: { children: JSX.Element }) {
  const location = useLocation();
  const isGoModeHost = () => hasNativeHostMarker() || hasHostModeQuery(location.query);
  const [nativeVoiceSessionConnected, setNativeVoiceSessionConnected] = createSignal(false);

  onMount(() => {
    if (!isGoModeHost()) return;
    const updateNativeVoiceState = () => setNativeVoiceSessionConnected(nativeVoiceConnected(window.goModeHost));
    updateNativeVoiceState();
    window.addEventListener("gomodevoicechange", updateNativeVoiceState);
    onCleanup(() => window.removeEventListener("gomodevoicechange", updateNativeVoiceState));
  });

  const hostMode: HostMode = {
    isGoModeHost,
    browserNotificationsEnabled: () => !isGoModeHost(),
    browserVoiceEnabled: () => !isGoModeHost(),
    nativeVoiceConnected: nativeVoiceSessionConnected,
  };

  return <HostModeContext.Provider value={hostMode}>{props.children}</HostModeContext.Provider>;
}

export function useHostMode(): HostMode {
  const hostMode = useContext(HostModeContext);
  if (!hostMode) throw new Error("useHostMode must be used within a HostModeProvider");
  return hostMode;
}
