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

// ConnectionStatus is the worst-wins ordering of the navbar connection dot:
// disconnected (red) > settled pass failed (orange) > settled pass in progress
// (yellow) > pass completed (green).
type ConnectionStatus = "disconnected" | "settled-error" | "settled-loading" | "connected";

function connectionStatus(connected: boolean, settledError: string, settledLoading: boolean): ConnectionStatus {
  if (!connected) return "disconnected";
  if (settledError !== "") return "settled-error";
  if (settledLoading) return "settled-loading";
  return "connected";
}

function ConnectionDot(props: { connected: boolean; settledLoading: boolean; settledError: string }) {
  const status = () => connectionStatus(props.connected, props.settledError, props.settledLoading);
  const classFor = (s: ConnectionStatus) => {
    switch (s) {
      case "disconnected": return styles.dotDisconnected;
      case "settled-error": return styles.dotSettledError;
      case "settled-loading": return styles.dotSettledLoading;
      case "connected": return styles.dotConnected;
    }
  };
  const titleFor = (s: ConnectionStatus) => {
    switch (s) {
      case "disconnected": return "Disconnected";
      case "settled-error": return props.settledError;
      case "settled-loading": return "Loading history…";
      case "connected": return "Connected";
    }
  };
  return (
    <span
      class={classFor(status())}
      title={titleFor(status())}
      data-status={status()}
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
        <header class={styles.navbar}>
          <h1 class={styles.title}>
            <button class={styles.titleButton} type="button" onClick={() => s.navigate("/")}>caic</button>
          </h1>
          <span class={styles.subtitle}>Coding Agents in Containers</span>
          <UsageBadges usage={s.usage} now={s.now} />
          <ConnectionDot connected={s.connected()} settledLoading={s.settledLoading()} settledError={s.settledError()} />
          <AccountMenu />
        </header>

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
