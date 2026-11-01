// Public pairing pages refresh the approving account/session in place and expose shared recovery with normal caic cookie sessions.

import { Show } from "solid-js";
import { DeviceLoginApproval, DeviceLoginDisplay } from "@maruel/gomode/web/DeviceLogin";
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
    if (!(await auth.refreshIdentity(signal)) || signal.aborted) return false;
    const current = auth.user();
    return (
      !!shown &&
      !!current &&
      current.id === shown.id &&
      current.sessionID === shown.sessionID &&
      current.username === shown.username &&
      current.provider === shown.provider
    );
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
        <Show when={auth.user()?.sessionID} fallback={<LoginPage next="/device" />}>
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
