// SessionsPage keeps session-list loading local and binds sign-out actions to their originating identity.

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
        !response.sessions?.some((session) => session.current && session.id === sid)
      ) {
        await auth.refreshIdentity();
      }
      return { sid, response };
    },
  );
  // Reading the resource directly would suspend the enclosing route during
  // refresh. Keep its last successful value and local loading/error feedback.
  const sessionData = () => {
    if (sessions.state !== "ready" && sessions.state !== "refreshing") return null;
    const data = sessions.latest;
    return data.sid === auth.user()?.sessionID ? data : null;
  };
  const [error, setError] = createSignal("");
  const [revoking, setRevoking] = createSignal("");
  let revokeOperation = 0;
  async function revoke(id: string, current: boolean) {
    const sid = auth.user()?.sessionID;
    if (!sid || !sessionData()) return;
    const isCurrent = () => !disposed && auth.ready() && auth.user()?.sessionID === sid;
    const operation = ++revokeOperation;
    const client = sessionClient(sid);
    setRevoking(id);
    setError("");
    try {
      await client.revokeSession(id);
      if (!isCurrent()) return;
      if (current) await auth.refreshIdentity();
      else await refetch();
    } catch (e) {
      if (!isCurrent()) return;
      if (e instanceof APIError && e.status === 401) {
        try {
          await auth.refreshIdentity();
        } catch (refreshError) {
          setError(refreshError instanceof Error ? refreshError.message : "Could not sign out the session.");
        }
      } else setError(e instanceof Error ? e.message : "Could not sign out the session.");
    } finally {
      if (!disposed && revokeOperation === operation) setRevoking("");
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
        <Show when={sessions.error as unknown}>
          <p role="alert">Could not load sessions.</p>
          <Button onClick={() => void refetch()}>Reload sessions</Button>
        </Show>
        <Show when={error()}>
          <p role="alert">{error()}</p>
        </Show>
        <Show when={!sessions.error}>
          <ul class={styles.list}>
            <For each={sessionData()?.response.sessions}>
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
