// Go Mode host-mode context: exposes native host capabilities from router state and native bridge.
import { createContext, useContext, type Accessor, type JSX } from "solid-js";
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
}

const HostModeContext = createContext<HostMode>();

function hasNativeHostMarker(): boolean {
  return window.goModeHost !== undefined;
}

export function hasHostModeQuery(query: SearchParams): boolean {
  const value = query[HOST_MODE_PARAM];
  return Array.isArray(value) ? value.includes("1") : value === "1";
}

export function HostModeProvider(props: { children: JSX.Element }) {
  const location = useLocation();
  const isGoModeHost = () => hasNativeHostMarker() || hasHostModeQuery(location.query);
  const hostMode: HostMode = {
    isGoModeHost,
    browserNotificationsEnabled: () => !isGoModeHost(),
    browserVoiceEnabled: () => !isGoModeHost(),
  };

  return <HostModeContext.Provider value={hostMode}>{props.children}</HostModeContext.Provider>;
}

export function useHostMode(): HostMode {
  const hostMode = useContext(HostModeContext);
  if (!hostMode) throw new Error("useHostMode must be used within a HostModeProvider");
  return hostMode;
}
