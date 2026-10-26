// SessionsPage binds tracked session actions to their originating identity and shows login origins and expiry.

import { createResource, createSignal, For, onCleanup, Show } from "solid-js";
import { APIError, createApiClient } from "@sdk/api.gen";
import { useAuth } from "../AuthContext";
import { Layout } from "../components/Layout";
import Button from "../components/Button";
import styles from "./SessionsPage.module.css";

export default function SessionsPage() {
  const auth = useAuth();
  let disposed = false;
  onCleanup(() => {
    disposed = true;
  });
  const sessionClient = (sid: string) =>
    createApiClient((url, init) => {
      const headers = new Headers(init?.headers);
      headers.set("X-Caic-CSRF", "1");
      headers.set("X-Caic-Session", sid);
      return fetch(url, { ...init, headers });
    });
  const [sessions, { refetch }] = createResource(
    () => auth.user()?.sessionID,
    async (sid) => {
      const response = await sessionClient(sid).listSessions();
      if (
        !disposed &&
        auth.user()?.sessionID === sid &&
        !response.sessions.some((session) => session.current && session.id === sid)
      ) {
        auth.reloadIdentity();
      }
      return { sid, response };
    },
  );
  const [error, setError] = createSignal("");
  const [revoking, setRevoking] = createSignal("");
  async function revoke(id: string, current: boolean) {
    const sid = auth.user()?.sessionID;
    if (!sid || sessions()?.sid !== sid) return;
    const isCurrent = () => !disposed && auth.ready() && auth.user()?.sessionID === sid;
    const client = sessionClient(sid);
    setRevoking(id);
    setError("");
    try {
      await client.revokeSession(id);
      if (!isCurrent()) return;
      if (current) auth.reloadIdentity();
      else await refetch();
    } catch (e) {
      if (!isCurrent()) return;
      if (e instanceof APIError && e.status === 401) auth.reloadIdentity();
      else setError(e instanceof Error ? e.message : "Could not sign out the session.");
    } finally {
      if (isCurrent()) setRevoking("");
    }
  }
  return (
    <Layout>
      <main class={styles.page}>
        <h1>Sessions</h1>
        <p>Each device has its own session. Sign out a device to end its cookie and API access.</p>
        <Show when={sessions.loading}>
          <p role="status">Loading sessions…</p>
        </Show>
        <Show when={sessions.error}>
          <p role="alert">Could not load sessions.</p>
          <Button onClick={() => void refetch()}>Reload sessions</Button>
        </Show>
        <Show when={error()}>
          <p role="alert">{error()}</p>
        </Show>
        <Show when={!sessions.error}>
          <ul class={styles.list}>
            <For each={sessions()?.sid === auth.user()?.sessionID ? sessions()?.response.sessions : []}>
              {(session) => (
                <li class={styles.session} data-testid={`session-${session.id}`}>
                  <div class={styles.details}>
                    <h2>
                      {session.source === "device" ? "Device sign-in" : "Provider sign-in"}
                      <Show when={session.current}> · This session</Show>
                    </h2>
                    <p>{session.userAgent || "Unknown browser"}</p>
                    <p>
                      {session.ip} · Signed in {new Date(session.createdAt).toLocaleString()}
                    </p>
                    <p>Expires {new Date(session.expiresAt).toLocaleString()}</p>
                  </div>
                  <Button
                    variant="red"
                    disabled={revoking() !== ""}
                    onClick={() => void revoke(session.id, session.current)}
                  >
                    {revoking() === session.id ? "Signing out…" : "Sign out session"}
                  </Button>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </main>
    </Layout>
  );
}
