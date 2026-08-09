// Application shell: top-level chrome, dialogs, diagnostic error boundary, Go Mode browser shell, and routed panes.

import { createEffect, createMemo, createSignal, ErrorBoundary, Show, type JSX } from "solid-js";

import GoModeBrowserShell from "./gomode/BrowserShell";
import { HostModeProvider } from "./gomode/HostMode";

import { currentErrorReport } from "./errorReport";
import { AppStateProvider, useAppState } from "./AppState";
import LoginPage from "./pages/LoginPage";
import AccountMenu from "./components/AccountMenu";
import Button from "./components/Button";
import ForkDialog from "./components/ForkDialog";
import Toasts from "./components/Toasts";
import UsageBadges from "./components/UsageBadges";
import CloneRepoDialog from "./components/CloneRepoDialog";
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
        <Button type="button" variant="gray" onClick={props.reset}>Try again</Button>
        <Button type="button" variant="gray" onClick={() => window.location.reload()}>Reload page</Button>
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

function ConnectionDot(props: { connected: boolean }) {
  return (
    <span
      class={props.connected ? styles.dotConnected : styles.dotDisconnected}
      title={props.connected ? "Connected" : "Disconnected"}
      data-testid="connection-dot"
    />
  );
}

/** Top-level chrome: navbar, modals, overlays, and the routed detail panes. */
function Shell(props: { children?: JSX.Element }) {
  const s = useAppState();
  const auth = s.auth;

  return (
    <Show when={auth.providers().length === 0 || auth.user()} fallback={<LoginPage />}>
      <div class={styles.app}>
        <div class={styles.navbar}>
          <h1 class={styles.title}>
            <button class={styles.titleButton} type="button" onClick={() => s.navigate("/")}>caic</button>
          </h1>
          <span class={styles.subtitle}>Coding Agents in Containers</span>
          <UsageBadges usage={s.usage} now={s.now} />
          <ConnectionDot connected={s.connected()} />
          <AccountMenu />
        </div>

        <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
          {props.children}
        </ErrorBoundary>

        <Show when={s.cloneOpen()}>
          <CloneRepoDialog
            loading={s.cloning()}
            error={s.cloneError()}
            onClone={s.submitClone}
            onClose={() => { s.setCloneOpen(false); s.setCloneError(""); }}
          />
        </Show>

        <ForkDialog />
        <GoModeBrowserShell />
        <Toasts />
      </div>
    </Show>
  );
}

/** Router layout for "/": provides the app store and renders the shell around routed panes. */
export default function App(props: { children?: JSX.Element }) {
  return (
    <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
      <HostModeProvider>
        <AppStateProvider>
          <Shell>{props.children}</Shell>
        </AppStateProvider>
      </HostModeProvider>
    </ErrorBoundary>
  );
}
