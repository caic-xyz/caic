// Auth context: tracks the current user and signals native voice when cookie identity changes.

import { createContext, createSignal, useContext, onMount, type ParentComponent } from "solid-js";

import type { AuthBootstrapResp, Config, UserResp } from "@sdk/types.gen";
import { APIError } from "@sdk/api.gen";
import { api } from "./api";

interface AuthState {
  /** True once the initial auth check has completed. */
  ready: () => boolean;
  /** Available OAuth providers, e.g. ["github", "gitlab"]; non-empty means auth is enabled. */
  providers: () => string[];
  /** The currently logged-in user, or null. */
  user: () => UserResp | null;
  /** Sign out and clear the session cookie. */
  logout: () => Promise<void>;
  /** Settle as signed out after /auth/me confirms the session is missing. */
  confirmLoggedOut: () => void;
}

declare global {
  interface Window {
    /** Auth state injected into the SPA document by the backend. */
    __CAIC_BOOTSTRAP__?: AuthBootstrapResp;
    /** Origin-scoped Android WebMessage listener, absent in ordinary browsers. */
    gomodeAuth?: { postMessage(message: string): void };
  }
}

const AuthContext = createContext<AuthState>();

function notifyNativeAuthChanged(nativeAccess: boolean): void {
  window.gomodeAuth?.postMessage(JSON.stringify({ authChanged: true, nativeAccess }));
}

function notifyNativeAuthChanging(): void {
  window.gomodeAuth?.postMessage(JSON.stringify({ authChanging: true }));
}

function isConfirmedLoggedOut(error: unknown): boolean {
  return error instanceof APIError && (error.status === 401 || error.status === 404);
}

export const AuthProvider: ParentComponent = (props) => {
  const [ready, setReady] = createSignal(false);
  const [providers, setProviders] = createSignal<string[]>([]);
  const [user, setUser] = createSignal<UserResp | null>(null);

  onMount(async () => {
    // The backend injects auth state into the served document so the logged-in
    // user is hydrated without a round-trip and the login page never flashes.
    const boot = window.__CAIC_BOOTSTRAP__;
    if (boot) {
      const authProviders = boot.authProviders ?? [];
      setProviders(authProviders);
      setUser(boot.user ?? null);
      setReady(true);
      notifyNativeAuthChanged(authProviders.length === 0 || (boot.user !== undefined && boot.user !== null));
      return;
    }
    // Fallback for documents served without injection (e.g. the Vite dev
    // server): fetch config from the public /server-info endpoint (the /api/
    // variant is session-gated and would 401 before login completes).
    let settled = false;
    let nativeAccess = false;
    try {
      const res = await fetch("/server-info/config");
      if (!res.ok) throw new Error(`server-info: ${res.status}`);
      const cfg: Config = await res.json();
      const authProviders = cfg.authProviders ?? [];
      setProviders(authProviders);
      if (authProviders.length === 0) {
        settled = true;
        nativeAccess = true;
      } else {
        try {
          const me = await api.getMe();
          setUser(me);
          settled = true;
          nativeAccess = true;
        } catch (error) {
          // /auth/me returns 404 when there is no user; 401 also confirms no session.
          settled = isConfirmedLoggedOut(error);
        }
      }
    } catch {
      // Server unreachable; auth state stays null.
    } finally {
      setReady(settled);
      if (settled) notifyNativeAuthChanged(nativeAccess);
    }
  });

  const logout = async () => {
    const previousUser = user();
    setReady(false);
    notifyNativeAuthChanging();
    try {
      await api.logout();
    } catch {
      try {
        const confirmedUser = await api.getMe();
        setUser(confirmedUser);
        setReady(true);
        notifyNativeAuthChanged(true);
      } catch (identityError) {
        if (isConfirmedLoggedOut(identityError)) {
          setUser(null);
          setReady(true);
          notifyNativeAuthChanged(false);
          return;
        }
        setUser(previousUser);
        // The cookie may have changed despite the network error; require a fresh bootstrap.
        return;
      }
      return;
    }
    setUser(null);
    setReady(true);
    notifyNativeAuthChanged(false);
  };

  const confirmLoggedOut = () => {
    setUser(null);
    setReady(true);
    notifyNativeAuthChanged(false);
  };

  return (
    <AuthContext.Provider value={{ ready, providers, user, logout, confirmLoggedOut }}>
      {props.children}
    </AuthContext.Provider>
  );
};

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}
