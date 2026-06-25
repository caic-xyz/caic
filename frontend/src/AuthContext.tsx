// Auth context: tracks the current user and auth configuration.

import { createContext, createSignal, useContext, onMount, type ParentComponent } from "solid-js";

import type { AuthBootstrapResp, Config, UserResp } from "@sdk/types.gen";

import { getMe, logout as apiLogout } from "./api";

interface AuthState {
  /** True once the initial auth check has completed. */
  ready: () => boolean;
  /** Available OAuth providers, e.g. ["github", "gitlab"]; non-empty means auth is enabled. */
  providers: () => string[];
  /** The currently logged-in user, or null. */
  user: () => UserResp | null;
  /** Sign out and clear the session cookie. */
  logout: () => Promise<void>;
  /** Clear the local user state without calling the API (e.g. on 401). */
  clearUser: () => void;
}

declare global {
  interface Window {
    /** Auth state injected into the SPA document by the backend. */
    __CAIC_BOOTSTRAP__?: AuthBootstrapResp;
  }
}

const AuthContext = createContext<AuthState>();

export const AuthProvider: ParentComponent = (props) => {
  const [ready, setReady] = createSignal(false);
  const [providers, setProviders] = createSignal<string[]>([]);
  const [user, setUser] = createSignal<UserResp | null>(null);

  onMount(async () => {
    // The backend injects auth state into the served document so the logged-in
    // user is hydrated without a round-trip and the login page never flashes.
    const boot = window.__CAIC_BOOTSTRAP__;
    if (boot) {
      setProviders(boot.authProviders ?? []);
      setUser(boot.user ?? null);
      setReady(true);
      return;
    }
    // Fallback for documents served without injection (e.g. the Vite dev
    // server): fetch config from the public /server-info endpoint (the /api/
    // variant is session-gated and would 401 before login completes).
    try {
      const res = await fetch("/server-info/config");
      if (!res.ok) throw new Error(`server-info: ${res.status}`);
      const cfg: Config = await res.json();
      const authProviders = cfg.authProviders ?? [];
      setProviders(authProviders);
      if (authProviders.length > 0) {
        try {
          const me = await getMe();
          setUser(me);
        } catch {
          // Not logged in.
        }
      }
    } catch {
      // Server unreachable; auth state stays null.
    } finally {
      setReady(true);
    }
  });

  const logout = async () => {
    await apiLogout();
    setUser(null);
  };

  const clearUser = () => setUser(null);

  return (
    <AuthContext.Provider value={{ ready, providers, user, logout, clearUser }}>
      {props.children}
    </AuthContext.Provider>
  );
};

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}
