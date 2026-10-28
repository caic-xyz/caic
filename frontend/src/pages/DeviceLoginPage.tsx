// Public pairing pages verify the approving account/session and expose shared recovery with normal caic cookie sessions.

import { Show } from "solid-js";
import { DeviceLoginApproval, DeviceLoginDisplay } from "@maruel/gomode/web/DeviceLogin";
import { APIError, createApiClient } from "@sdk/api.gen";
import { useAuth } from "../AuthContext";
import LoginPage from "./LoginPage";
import styles from "./DeviceLoginPage.module.css";

const endpoints = {
  start: "/auth/device/start",
  poll: "/auth/device/poll",
  lookup: "/auth/device/lookup",
  approve: "/auth/device/approve",
};

export function DeviceLoginPage() {
  const auth = useAuth();
  return (
    <main class={styles.page}>
      <h1>Sign in to caic</h1>
      <Show when={auth.ready()} fallback={<p role="status">Checking session…</p>}>
        <Show when={auth.providers().length > 0} fallback={<p>Device sign-in is unavailable.</p>}>
          <DeviceLoginDisplay
            options={{
              endpoints,
              onCheckSession: () => window.location.assign("/"),
              onSession: () => {
                auth.reloadIdentity("/");
              },
            }}
          />
        </Show>
      </Show>
    </main>
  );
}

export function DeviceApprovalPage() {
  const auth = useAuth();
  const verifyApprover = async (signal: AbortSignal) => {
    const shown = auth.user();
    try {
      const current = await createApiClient((url, init) => fetch(url, { ...init, cache: "no-store", signal })).getMe();
      if (signal.aborted) return false;
      if (
        !shown ||
        current.id !== shown.id ||
        current.sessionID !== shown.sessionID ||
        current.username !== shown.username ||
        current.provider !== shown.provider
      ) {
        auth.reloadIdentity();
        return false;
      }
      return true;
    } catch (error) {
      if (signal.aborted) return false;
      if (error instanceof APIError && (error.status === 401 || error.status === 404)) {
        auth.reloadIdentity();
        return false;
      }
      throw error;
    }
  };
  return (
    <Show when={auth.ready()} fallback={<p role="status">Checking session…</p>}>
      <Show
        when={auth.providers().length > 0}
        fallback={
          <main class={styles.page}>
            <p>Device sign-in is unavailable.</p>
          </main>
        }
      >
        <Show when={auth.user()?.sessionID} keyed fallback={<LoginPage next="/device" />}>
          <main class={styles.page}>
            <h1>Approve device sign-in</h1>
            <p>
              Signed in as {auth.user()?.username} ({auth.user()?.provider}).
            </p>
            <button type="button" class={styles.accountButton} onClick={() => void auth.logout()}>
              Use another account
            </button>
            <DeviceLoginApproval
              endpoints={endpoints}
              headers={() => ({ "X-Caic-Session": auth.user()?.sessionID ?? "" })}
              onAuthenticationRequired={auth.reloadIdentity}
              verifyApprover={verifyApprover}
            />
          </main>
        </Show>
      </Show>
    </Show>
  );
}
