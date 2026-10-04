// Application shell: top-level chrome, dialogs, mobile voice view, diagnostic error boundary, and routed panes.

import { createEffect, createMemo, createSignal, ErrorBoundary, onCleanup, onMount, Show, type JSX } from "solid-js";
import { useLocation } from "@solidjs/router";

import VoiceTaskFocus from "./VoiceTaskFocus";
import BrowserVoiceShell from "./BrowserVoiceShell";
import { HostModeProvider, useHostMode } from "@maruel/gomode/web/HostMode";

import { currentErrorReport } from "./errorReport";
import { AppStateProvider, useAppState } from "./AppState";
import { useAuth } from "./AuthContext";
import LoginPage from "./pages/LoginPage";
import AccountMenu from "./components/AccountMenu";
import Button from "./components/Button";
import ForkDialog from "./components/ForkDialog";
import KeyboardShortcuts from "./components/KeyboardShortcuts";
import Toasts from "./components/Toasts";
import UsageBadges from "./components/UsageBadges";
import CloneRepoDialog from "./components/CloneRepoDialog";
import MobileVoiceTasks from "./components/MobileVoiceTasks";
import { getVoiceTaskNumber, voiceConnected } from "./voiceTaskState";
import styles from "./App.module.css";

/** Fallback UI shown when an ErrorBoundary catches a render error. */
function ErrorFallback(props: { error: unknown; reset: () => void }) {
  const [copyError, setCopyError] = createSignal("");
  const [copied, setCopied] = createSignal(false);
  const message = () => (props.error instanceof Error ? props.error.message : String(props.error));
  const report = createMemo(() => currentErrorReport(props.error));
  createEffect(() => console.error("caic frontend ErrorBoundary caught a render error.\n" + report()));

  async function copyDiagnosticDetails() {
    try {
      await navigator.clipboard.writeText(report());
      setCopied(true);
      setCopyError("");
    } catch (error) {
      console.error("Failed to copy caic frontend diagnostic details", error);
      setCopyError("Could not copy details. Select and copy them below.");
    }
  }

  return (
    <div class={styles.errorFallback} role="alert" data-testid="error-fallback">
      <p class={styles.errorTitle}>Something went wrong.</p>
      <pre class={styles.errorMessage}>{message()}</pre>
      <div class={styles.errorActions}>
        <Button type="button" variant="gray" onClick={props.reset}>
          Try again
        </Button>
        <Button type="button" variant="gray" onClick={() => window.location.reload()}>
          Reload page
        </Button>
        <Button type="button" variant="gray" onClick={() => void copyDiagnosticDetails()}>
          {copied() ? "Copied details" : "Copy diagnostic details"}
        </Button>
      </div>
      <Show when={copyError()}>
        <p class={styles.errorCopyFailure}>{copyError()}</p>
      </Show>
      <details class={styles.errorDetails}>
        <summary>Technical details</summary>
        <pre class={styles.errorReport}>{report()}</pre>
      </details>
    </div>
  );
}

// ConnectionStatus is the worst-wins ordering of the navbar wordmark:
// disconnected or failed (red) > loading (amber) > connected (black).
type ConnectionStatus = "disconnected" | "settled-error" | "settled-loading" | "connected";

function connectionStatus(connected: boolean, settledError: string, settledLoading: boolean): ConnectionStatus {
  if (!connected) return "disconnected";
  if (settledError !== "") return "settled-error";
  if (settledLoading) return "settled-loading";
  return "connected";
}

function connectionStatusLabel(status: ConnectionStatus, settledError: string): string {
  switch (status) {
    case "disconnected":
      return "Disconnected";
    case "settled-error":
      return settledError;
    case "settled-loading":
      return "Loading history…";
    case "connected":
      return "Connected";
  }
}

/** Holds the screen while cookie identity is unresolved and offers a reload. */
function SessionCheckPage() {
  return (
    <div class="login-page">
      <div class="login-card" role="status">
        <h1 class="login-title">caic</h1>
        <p class="login-subtitle">Checking your session…</p>
        <button type="button" class="login-button" onClick={() => window.location.reload()}>
          Retry
        </button>
      </div>
    </div>
  );
}

/** Top-level chrome: navbar, modals, overlays, and the routed detail panes. */
function Shell(props: { children?: JSX.Element }) {
  const s = useAppState();
  const location = useLocation();
  const hostMode = useHostMode();
  const auth = s.auth;
  const [shortcutsOpen, setShortcutsOpen] = createSignal(false);
  const status = () => connectionStatus(s.connected(), s.settledError(), s.settledLoading());
  const statusLabel = () => connectionStatusLabel(status(), s.settledError());
  const mobileQuery = window.matchMedia("(max-width: 768px)");
  const [mobile, setMobile] = createSignal(mobileQuery.matches);
  const mobileVoice = () => mobile() && voiceConnected() && location.pathname === "/";

  onMount(() => {
    const onViewportChange = (event: MediaQueryListEvent) => setMobile(event.matches);
    mobileQuery.addEventListener("change", onViewportChange);
    onCleanup(() => mobileQuery.removeEventListener("change", onViewportChange));
  });

  return (
    <Show when={auth.ready() || auth.providers().length === 0} fallback={<SessionCheckPage />}>
      <Show when={auth.providers().length === 0 || auth.user()} fallback={<LoginPage />}>
        <div class={styles.app} data-testid="app-shell">
          <div
            class={`${styles.normalContent} ${mobileVoice() ? styles.voiceHidden : ""}`}
            aria-hidden={mobileVoice()}
            data-testid="normal-content"
          >
            <header class={styles.navbar}>
              <h1 class={styles.title}>
                <button
                  class={styles.titleButton}
                  type="button"
                  onClick={() => s.navigate("/")}
                  title={`New task — ${statusLabel()}`}
                  aria-describedby="connection-status"
                  data-status={status()}
                  data-testid="new-task-button"
                >
                  caic
                </button>
              </h1>
              <span id="connection-status" class={styles.visuallyHidden} aria-live="polite">
                {statusLabel()}
              </span>
              <span class={styles.subtitle}>Coding Agents in Containers</span>
              <UsageBadges usage={s.usage} now={s.now} />
              <AccountMenu onKeyboardShortcuts={() => setShortcutsOpen(true)} />
            </header>

            <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
              {props.children}
            </ErrorBoundary>
          </div>

          <Show when={mobileVoice()}>
            <MobileVoiceTasks
              tasks={s.tasks}
              tasksLoading={s.tasksLoading}
              settledLoading={s.settledLoading}
              getTaskNumber={getVoiceTaskNumber}
            />
          </Show>

          <Show when={s.cloneOpen()}>
            <CloneRepoDialog
              loading={s.cloning()}
              error={s.cloneError()}
              onClone={s.submitClone}
              onClose={() => {
                s.setCloneOpen(false);
                s.setCloneError("");
              }}
            />
          </Show>

          <ForkDialog />
          <KeyboardShortcuts
            open={shortcutsOpen()}
            onOpenChange={setShortcutsOpen}
            voiceAvailable={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable()}
          />
          <VoiceTaskFocus mobile={mobile} />
          <BrowserVoiceShell />
          <Toasts />
        </div>
      </Show>
    </Show>
  );
}

/** Router layout for "/": provides the app store and renders the shell around routed panes. */
export default function App(props: { children?: JSX.Element }) {
  const auth = useAuth();
  const accountScope = () => {
    if (auth.providers().length === 0) return "auth-disabled";
    if (!auth.ready()) return null;
    const user = auth.user();
    return user ? `${user.provider}:${user.id}` : null;
  };

  return (
    <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
      <HostModeProvider>
        <Show
          when={accountScope()}
          keyed
          fallback={
            <Show when={auth.ready()} fallback={<SessionCheckPage />}>
              <LoginPage />
            </Show>
          }
        >
          {(_scope) => (
            <AppStateProvider>
              <Shell>{props.children}</Shell>
            </AppStateProvider>
          )}
        </Show>
      </HostModeProvider>
    </ErrorBoundary>
  );
}
