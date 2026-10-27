// Owns cookie identity, page-bound frontend preparation, and account reload/logout transitions.

import { batch, createContext, createSignal, useContext, onCleanup, onMount, type ParentComponent } from "solid-js";

import type { AuthBootstrapResp, Config, UserResp } from "@sdk/types.gen";
import { APIError } from "@sdk/api.gen";
import { api } from "./api";
import { createFrontendPreparationClient, type FrontendPreparation } from "@maruel/gomode/web/FrontendVoiceTools";

interface AuthState {
  /** True once the initial auth check has completed. */
  ready: () => boolean;
  /** Available OAuth providers, e.g. ["github", "gitlab"]; non-empty means auth is enabled. */
  providers: () => string[];
  /** The currently logged-in user, or null. */
  user: () => UserResp | null;
  frontendPreparation: () => FrontendPreparation | null;
  /** Sign out and clear the session cookie. */
  logout: () => Promise<void>;
  /** Settle as signed out after /auth/me confirms the session is missing. */
  confirmLoggedOut: () => void;
  /** Invalidate native identity before reloading the current cookie session. */
  reloadIdentity: (destination?: "/") => void;
}

declare global {
  interface Window {
    /** Auth state injected into the SPA document by the backend. */
    __CAIC_BOOTSTRAP__?: AuthBootstrapResp;
  }
}

const AuthContext = createContext<AuthState>();

function isConfirmedLoggedOut(error: unknown): boolean {
  return error instanceof APIError && (error.status === 401 || error.status === 404);
}

export const AuthProvider: ParentComponent = (props) => {
  const nativeFrontend = createFrontendPreparationClient();
  const [ready, setReady] = createSignal(false);
  const [providers, setProviders] = createSignal<string[]>([]);
  const [user, setUser] = createSignal<UserResp | null>(null);
  const [frontendPreparation, setFrontendPreparation] = createSignal<FrontendPreparation | null>(null);
  let disposed = false;

  function publishAuth(message: Record<string, unknown>) {
    if (disposed) return;
    frontendPreparation()?.dispose();
    setFrontendPreparation(nativeFrontend.publishAuth(message));
  }

  function settleIdentity(currentUser: UserResp | null, nativeAccess: boolean) {
    if (disposed) return;
    batch(() => {
      publishAuth({ authChanged: true, nativeAccess });
      setUser(currentUser);
      setReady(true);
    });
  }

  onCleanup(() => {
    disposed = true;
    frontendPreparation()?.dispose();
  });

  onMount(async () => {
    // The backend injects auth state into the served document so the logged-in
    // user is hydrated without a round-trip and the login page never flashes.
    const boot = window.__CAIC_BOOTSTRAP__;
    if (boot) {
      const authProviders = boot.authProviders ?? [];
      setProviders(authProviders);
      settleIdentity(boot.user ?? null, authProviders.length === 0 || (boot.user !== undefined && boot.user !== null));
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
      if (settled) settleIdentity(user(), nativeAccess);
    }
  });

  const logout = async () => {
    if (disposed) return;
    const previousUser = user();
    const sessionID = previousUser?.sessionID;
    if (!sessionID) {
      reloadIdentity();
      return;
    }
    batch(() => {
      publishAuth({ authChanging: true });
      setReady(false);
    });
    try {
      await api.logout(sessionID);
    } catch (error) {
      if (disposed) return;
      if (error instanceof APIError && error.status === 401) {
        reloadIdentity();
        return;
      }
      try {
        const confirmedUser = await api.getMe();
        settleIdentity(confirmedUser, true);
      } catch (identityError) {
        if (isConfirmedLoggedOut(identityError)) {
          settleIdentity(null, false);
          return;
        }
        setUser(previousUser);
        // The cookie may have changed despite the network error; require a fresh bootstrap.
        return;
      }
      return;
    }
    settleIdentity(null, false);
  };

  const confirmLoggedOut = () => {
    settleIdentity(null, false);
  };

  const reloadIdentity = (destination?: "/") => {
    if (disposed) return;
    if (ready()) publishAuth({ authChanging: true });
    setReady(false);
    if (destination) window.location.assign(destination);
    else window.location.reload();
  };

  return (
    <AuthContext.Provider
      value={{ ready, providers, user, frontendPreparation, logout, confirmLoggedOut, reloadIdentity }}
    >
      {props.children}
    </AuthContext.Provider>
  );
};

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}
