// Loads, saves, and removes the server's Tailscale API access token on behalf of an administrator.

import { createSignal, type Accessor } from "solid-js";
import { APIError } from "@sdk/api.gen";
import type { TailscaleKeyResp } from "@sdk/types.gen";
import { api } from "./api";

/** Tailscale admin console page that creates API access tokens. */
export const TAILSCALE_KEYS_URL = "https://console.tailscale.com/admin/settings/keys";

export interface TailscaleKey {
  /** The active token's description; null until loaded and for non-administrators. */
  status: Accessor<TailscaleKeyResp | null>;
  /** Why the description could not be loaded. */
  loadError: Accessor<string>;
  pending: Accessor<"save" | "remove" | null>;
  /** Why the latest save or removal failed; cleared by the next one or by clearActionError. */
  actionError: Accessor<string>;
  clearActionError: () => void;
  load: () => Promise<void>;
  /** Resolves true once the server verified and saved apiKey. */
  save: (apiKey: string) => Promise<boolean>;
  remove: () => Promise<void>;
}

/**
 * createTailscaleKey owns one administrator view of the token. onChange receives
 * every description the server returns so task creation follows the new token.
 * A response superseded by a later request is dropped.
 */
export function createTailscaleKey(onChange: (status: TailscaleKeyResp) => void): TailscaleKey {
  const [status, setStatus] = createSignal<TailscaleKeyResp | null>(null);
  const [loadError, setLoadError] = createSignal("");
  const [pending, setPending] = createSignal<"save" | "remove" | null>(null);
  const [actionError, setActionError] = createSignal("");
  let generation = 0;

  const apply = (resp: TailscaleKeyResp) => {
    setStatus(resp);
    setLoadError("");
    onChange(resp);
  };

  async function load() {
    const gen = ++generation;
    try {
      const resp = await api.getTailscaleKey();
      if (gen === generation) apply(resp);
    } catch (e: unknown) {
      if (gen !== generation) return;
      setStatus(null);
      // Only administrators manage the token; others see no section.
      setLoadError(e instanceof APIError && e.status === 403 ? "" : errorMessage(e));
    }
  }

  async function act(kind: "save" | "remove", call: () => Promise<TailscaleKeyResp>): Promise<boolean> {
    const gen = ++generation;
    setPending(kind);
    setActionError("");
    try {
      const resp = await call();
      if (gen === generation) apply(resp);
      return true;
    } catch (e: unknown) {
      if (gen === generation) setActionError(errorMessage(e));
      return false;
    } finally {
      if (gen === generation) setPending(null);
    }
  }

  return {
    status,
    loadError,
    pending,
    actionError,
    clearActionError: () => setActionError(""),
    load,
    save: (apiKey) => act("save", () => api.setTailscaleKey({ apiKey: apiKey.trim() })),
    remove: async () => {
      await act("remove", () => api.removeTailscaleKey());
    },
  };
}

function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** availableUntilMs maps a server deadline to milliseconds: null when unavailable, Infinity when it never ends. */
export function availableUntilMs(available: boolean, until: string | undefined): number | null {
  if (!available) return null;
  return until ? Date.parse(until) : Number.POSITIVE_INFINITY;
}

const units: Array<[Intl.RelativeTimeFormatUnit, number]> = [
  ["day", 86_400_000],
  ["hour", 3_600_000],
  ["minute", 60_000],
];

/** formatRemaining describes how far expiresAt is from now, such as "in 23 days" or "2 hours ago". */
export function formatRemaining(expiresAt: number, now: number): string {
  const delta = expiresAt - now;
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  for (const [unit, ms] of units) {
    // Two of a unit before switching to it keeps "in 36 hours" more precise than "in 2 days".
    if (Math.abs(delta) >= 2 * ms || unit === "minute") return rtf.format(Math.trunc(delta / ms), unit);
  }
  return "";
}
