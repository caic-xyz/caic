// SettingsForm presents autosaved container, storage, automation, and server settings.

import { useSearchParams } from "@solidjs/router";
import { createEffect, createMemo, For, Match, onCleanup, Show, Switch, type Accessor, type Setter } from "solid-js";

import type {
  CacheMappingResp,
  CacheSize,
  Harness,
  HarnessInfo,
  ImageRefreshStatus,
  OAuthGrantResp,
  MountMappingResp,
  Platform,
  RuntimeInfo,
  RuntimeSettings,
  UpdatePreferencesReq,
  VersionResp,
  WellKnownCachesResp,
} from "@sdk/types.gen";
import CloudDoneIcon from "@material-symbols/svg-400/outlined/cloud_done.svg?solid";
import CloudOffIcon from "@material-symbols/svg-400/outlined/cloud_off.svg?solid";
import CloudUploadIcon from "@material-symbols/svg-400/outlined/cloud_upload.svg?solid";
import EditNoteIcon from "@material-symbols/svg-400/outlined/edit_note.svg?solid";
import ErrorIcon from "@material-symbols/svg-400/outlined/error.svg?solid";
import type { AppStore } from "../AppState";
import Button from "./Button";
import SettingsMappings, { containerSortPath } from "./SettingsMappings";

import { formatDuration, parseDuration } from "../duration";

import styles from "./SettingsForm.module.css";

type SettingsOverrides = Partial<UpdatePreferencesReq["settings"]>;

interface SettingsFormProps {
  selectedImage: Accessor<string>;
  setSelectedImage: Setter<string>;
  runtimeSettings: Accessor<Record<string, RuntimeSettings | undefined>>;
  updateRuntimeSettings: (name: string, settings: Partial<RuntimeSettings>) => void;
  purgeDelay: Accessor<number>;
  setPurgeDelay: Setter<number>;
  runtimes: Accessor<RuntimeInfo[]>;
  harnesses: Accessor<HarnessInfo[]>;
  selectedRuntimeName: Accessor<string>;
  setSelectedRuntimeName: (runtimeName: string) => void;
  wellKnownCaches: Accessor<Record<string, boolean | undefined>>;
  setWellKnownCaches: Setter<Record<string, boolean | undefined>>;
  wellKnownCachesList: Accessor<WellKnownCachesResp["wellKnown"]>;
  wellKnownCacheSizes: Accessor<Record<string, CacheSize | undefined>>;
  cacheMappings: Accessor<CacheMappingResp[]>;
  setCacheMappings: Setter<CacheMappingResp[]>;
  customMounts: Accessor<MountMappingResp[]>;
  setCustomMounts: Setter<MountMappingResp[]>;
  settingsError: Accessor<string>;
  settingsSaveState: AppStore["settingsSaveState"];
  markSettingsDraft: AppStore["markSettingsDraft"];
  autoFixCI: Accessor<boolean>;
  setAutoFixCI: Setter<boolean>;
  autoFixPR: Accessor<boolean>;
  setAutoFixPR: Setter<boolean>;
  mcpOAuthAvailable: Accessor<boolean>;
  oauthGrants: Accessor<OAuthGrantResp[]>;
  oauthGrantError: Accessor<string>;
  revokingOAuthGrantID: Accessor<string | null>;
  revokeOAuthClientGrant: (grantID: string) => Promise<void>;
  versionInfo: Accessor<VersionResp | null>;
  versionCheckError: Accessor<string>;
  checkingUpdate: Accessor<boolean>;
  updating: Accessor<boolean>;
  updateStatus: Accessor<string>;
  refreshingHarness: Accessor<Harness | null>;
  modelRefreshStatus: AppStore["modelRefreshStatus"];
  imageRefreshStatus: (runtimeName: string) => ImageRefreshStatus;
  startImageRefresh: (runtimeName: string) => Promise<void>;
  saveSettings: (overrides?: SettingsOverrides) => Promise<void>;
  triggerServerUpdate: () => Promise<void>;
  refreshAvailableModels: (harness: Harness) => Promise<void>;
}

export default function SettingsForm(props: SettingsFormProps) {
  const [search] = useSearchParams();
  const section = () => (search.section === "storage" || search.section === "server" ? search.section : "general");
  const drafts = new Set<HTMLInputElement>();
  // Track DOM drafts too: invalid durations and unblurred fields are not yet
  // represented in the preference payload. Section changes discard those DOM drafts.
  createEffect(() => {
    section();
    drafts.clear();
    props.markSettingsDraft(false);
  });
  onCleanup(() => props.markSettingsDraft(false));
  const saveLabel = () =>
    props.settingsError()
      ? "Settings not saved"
      : props.settingsSaveState() === "dirty"
        ? "Unsaved settings"
        : props.settingsSaveState() === "saving"
          ? "Saving settings…"
          : props.settingsSaveState() === "saved"
            ? "Settings saved"
            : "";
  const refreshableHarnesses = () => props.harnesses().filter((harness) => harness.supportsModelRefresh);
  const formatBytes = (bytes: number): string => {
    if (bytes <= 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    const i = Math.min(Math.floor(Math.log2(bytes) / 10), units.length - 1);
    const value = bytes / 1024 ** i;
    return `${value >= 10 || i === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[i]}`;
  };
  const cacheSizeLabel = (name: string): string => {
    const size = props.wellKnownCacheSizes()[name];
    if (!size) return "pending";
    if (size.error) return "error";
    return formatBytes(size.sizeBytes ?? 0);
  };
  const formatDate = (value?: string): string => {
    if (!value) return "Never";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "Unknown";
    return date.toLocaleString();
  };
  const caches = createMemo(() =>
    [...props.wellKnownCachesList()].sort((a, b) => {
      const firstPath = (mounts: string[]) => mounts.map(containerSortPath).sort()[0] ?? "";
      const left = firstPath(a.mounts);
      const right = firstPath(b.mounts);
      return left < right ? -1 : left > right ? 1 : a.name.localeCompare(b.name);
    }),
  );

  return (
    <div class={styles.settingsPage}>
      <div
        class={styles.settingsPanel}
        data-section={section()}
        onInput={(event) => {
          const input = event.target;
          if (input instanceof HTMLInputElement && (input.type === "text" || input.type === "number")) {
            drafts.add(input);
            props.markSettingsDraft(true);
          }
        }}
        onFocusOut={(event) => {
          const input = event.target;
          if (input instanceof HTMLInputElement && input.validity.valid && drafts.delete(input)) {
            props.markSettingsDraft(drafts.size > 0);
          }
        }}
      >
        <div class={styles.settingsHeader}>
          <h2 class={styles.settingsPanelTitle}>Settings</h2>
          <span
            class={styles.saveStatus}
            role="status"
            aria-label="Settings save status"
            title={saveLabel()}
            data-state={props.settingsError() ? "error" : props.settingsSaveState()}
          >
            <Switch>
              <Match when={props.settingsError()}>
                <CloudOffIcon aria-hidden="true" />
              </Match>
              <Match when={props.settingsSaveState() === "saving"}>
                <CloudUploadIcon aria-hidden="true" />
              </Match>
              <Match when={props.settingsSaveState() === "dirty"}>
                <EditNoteIcon aria-hidden="true" />
              </Match>
              <Match when={props.settingsSaveState() === "saved"}>
                <CloudDoneIcon aria-hidden="true" />
              </Match>
            </Switch>
            <span class={styles.saveMessage}>{saveLabel()}</span>
          </span>
        </div>
        <nav class={styles.sectionNav} aria-label="Settings sections">
          <a href="/settings" aria-current={section() === "general" ? "page" : undefined}>
            General
          </a>
          <a href="/settings?section=storage" aria-current={section() === "storage" ? "page" : undefined}>
            Storage
          </a>
          <a href="/settings?section=server" aria-current={section() === "server" ? "page" : undefined}>
            Server
          </a>
        </nav>
        <Show when={props.settingsError()}>
          <p class={styles.settingsError} role="alert">
            {props.settingsError()}
          </p>
        </Show>
        <Show when={section() === "general"}>
          <section class={styles.settingsSection} aria-labelledby="settings-container">
            <h3 id="settings-container" class={styles.settingsSectionTitle}>
              Container
            </h3>
            <label class={styles.settingsLabel}>
              Docker image
              <input
                type="text"
                class={styles.settingsInput}
                placeholder="ghcr.io/caic-xyz/md-user:latest"
                value={props.selectedImage() || ""}
                onChange={(e) => props.setSelectedImage(e.currentTarget.value)}
                onBlur={(e) => {
                  if (drafts.has(e.currentTarget)) void props.saveSettings();
                }}
              />
            </label>
            <Show when={props.runtimes().length > 1}>
              <label class={styles.settingsLabel}>
                Default runtime
                <select
                  class={styles.settingsInput}
                  value={props.selectedRuntimeName()}
                  onChange={(e) => {
                    const runtimeName = e.currentTarget.value;
                    props.setSelectedRuntimeName(runtimeName);
                    void props.saveSettings({ runtimeName });
                  }}
                >
                  <For each={props.runtimes()}>
                    {(rt) => (
                      <option value={rt.name} selected={rt.name === props.selectedRuntimeName()}>
                        {rt.name}
                      </option>
                    )}
                  </For>
                </select>
              </label>
            </Show>
            <For each={props.runtimes()}>
              {(rt) => (
                <fieldset class={styles.runtimeSettings}>
                  <legend>{rt.name}</legend>
                  <label class={styles.settingsLabel}>
                    CPU architecture
                    <select
                      class={styles.settingsInput}
                      value={props.runtimeSettings()[rt.name]?.containerPlatform ?? ""}
                      onChange={(e) => {
                        props.updateRuntimeSettings(rt.name, { containerPlatform: e.currentTarget.value as Platform });
                        void props.saveSettings();
                      }}
                    >
                      <option value="">Native</option>
                      <option value="linux/amd64">linux/amd64</option>
                      <option value="linux/arm64">linux/arm64</option>
                    </select>
                  </label>
                  <label class={`${styles.settingsLabel} ${styles.shortField}`}>
                    CPU cores
                    <input
                      type="number"
                      class={styles.settingsInput}
                      placeholder="Automatic"
                      min="0"
                      step="1"
                      value={props.runtimeSettings()[rt.name]?.maxCPUs || ""}
                      onChange={(e) =>
                        props.updateRuntimeSettings(rt.name, { maxCPUs: parseInt(e.currentTarget.value, 10) || 0 })
                      }
                      onBlur={(e) => {
                        if (drafts.has(e.currentTarget)) void props.saveSettings();
                      }}
                    />
                  </label>
                  <div class={styles.runtimeRefresh}>
                    <Button
                      type="button"
                      variant="gray"
                      aria-label={`Refresh image and coding agents for ${rt.name}`}
                      disabled={props.imageRefreshStatus(rt.name).state === "running"}
                      loading={
                        props.imageRefreshStatus(rt.name).state === "running" &&
                        !props.imageRefreshStatus(rt.name).scheduled
                      }
                      onClick={() => void props.startImageRefresh(rt.name)}
                    >
                      Refresh image and coding agents
                    </Button>
                    <Show when={props.imageRefreshStatus(rt.name).state !== "idle"}>
                      <p class={styles.settingsDescription} role="status">
                        {props.imageRefreshStatus(rt.name).state === "running"
                          ? props.imageRefreshStatus(rt.name).scheduled
                            ? "Scheduled image warmup is running on this runtime…"
                            : "Refreshing image and coding agents…"
                          : props.imageRefreshStatus(rt.name).state === "succeeded"
                            ? "Image and coding agents refreshed. New tasks will use the updated image."
                            : `Image refresh failed: ${props.imageRefreshStatus(rt.name).error ?? "Unknown error"}`}
                      </p>
                    </Show>
                  </div>
                </fieldset>
              )}
            </For>
          </section>
        </Show>
        <Show when={section() === "storage"}>
          <section class={styles.settingsSection} aria-labelledby="settings-well-known-caches">
            <h3 id="settings-well-known-caches" class={styles.settingsSectionTitle}>
              Well-known caches
            </h3>
            <div class={styles.cacheGrid}>
              <For each={caches()}>
                {(cache) => {
                  const state = () => props.wellKnownCaches()[cache.name];
                  const isEnabled = () => state() === true;
                  return (
                    <label
                      class={styles.cacheCheckbox}
                      data-state={isEnabled() ? "enabled" : "disabled"}
                      title={cache.description}
                    >
                      <input
                        type="checkbox"
                        aria-label={cache.name}
                        checked={isEnabled()}
                        onChange={(e) => {
                          const newCaches = { ...props.wellKnownCaches() };
                          newCaches[cache.name] = e.currentTarget.checked;
                          props.setWellKnownCaches(newCaches);
                          void props.saveSettings({
                            wellKnownCaches: newCaches as Record<string, boolean>,
                          });
                        }}
                      />
                      <span class={styles.cacheInfo}>
                        <span class={styles.cacheName}>{cache.name}</span>
                        <span class={styles.cachePath}>
                          {[...cache.mounts]
                            .sort((a, b) => containerSortPath(a).localeCompare(containerSortPath(b)))
                            .map((path) => path.replace(/^\/home\/user(?=\/|$)/, "~"))
                            .join(" · ")}
                        </span>
                      </span>
                      <span class={styles.cacheSize} data-testid="cache-size">
                        {cacheSizeLabel(cache.name)}
                      </span>
                    </label>
                  );
                }}
              </For>
            </div>
          </section>
          <section class={styles.settingsSection} aria-labelledby="settings-custom-caches">
            <h3 id="settings-custom-caches" class={styles.settingsSectionTitle}>
              Custom caches
            </h3>
            <p class={styles.settingsDescription}>Directories copied into the container</p>
            <SettingsMappings
              isDirty={(input) => drafts.has(input)}
              items={props.cacheMappings()}
              mounts={false}
              setItems={props.setCacheMappings}
              save={(cacheMappings) => {
                void props.saveSettings({ cacheMappings });
              }}
            />
          </section>
          <section class={styles.settingsSection} aria-labelledby="settings-custom-mounts">
            <h3 id="settings-custom-mounts" class={styles.settingsSectionTitle}>
              Custom mounts
            </h3>
            <p class={styles.settingsDescription}>Directories directly available to the container</p>
            <SettingsMappings
              isDirty={(input) => drafts.has(input)}
              items={props.customMounts()}
              mounts={true}
              setItems={props.setCustomMounts}
              save={(customMounts) => {
                void props.saveSettings({ customMounts });
              }}
            />
          </section>
        </Show>
        <Show when={section() === "server" && props.mcpOAuthAvailable()}>
          <section class={styles.settingsSection} aria-labelledby="settings-mcp-clients">
            <h3 id="settings-mcp-clients" class={styles.settingsSectionTitle}>
              MCP clients
            </h3>
            <p class={styles.settingsDescription}>Clients with access to caic.</p>
            <Show
              when={!props.oauthGrantError()}
              fallback={
                <p class={`${styles.settingsDescription} ${styles.settingsDescriptionError}`}>
                  {props.oauthGrantError()}
                </p>
              }
            >
              <Show
                when={props.oauthGrants().length > 0}
                fallback={<p class={styles.settingsDescription}>No connected MCP clients.</p>}
              >
                <div class={styles.oauthGrantList}>
                  <For each={props.oauthGrants()}>
                    {(grant) => (
                      <div class={styles.oauthGrantCard} data-status={grant.status}>
                        <div class={styles.oauthGrantHeader}>
                          <div>
                            <div class={styles.oauthGrantName}>{grant.clientName || grant.clientID}</div>
                            <div class={styles.oauthGrantMeta}>{grant.clientID}</div>
                          </div>
                          <span class={styles.oauthGrantStatus}>{grant.status}</span>
                        </div>
                        <div class={styles.oauthGrantDetails}>
                          <div>Scopes: {grant.scopes.join(", ")}</div>
                          <div>Resource: {grant.resource}</div>
                          <div>Created: {formatDate(grant.createdAt)}</div>
                          <div>Last used: {formatDate(grant.lastUsedAt)}</div>
                          <div>Expires: {formatDate(grant.expiresAt)}</div>
                        </div>
                        <Show when={grant.status !== "revoked"}>
                          <button
                            type="button"
                            class={styles.settingsButton}
                            disabled={props.revokingOAuthGrantID() === grant.id}
                            onClick={() => {
                              void props.revokeOAuthClientGrant(grant.id);
                            }}
                          >
                            {props.revokingOAuthGrantID() === grant.id ? "Revoking…" : "Revoke access"}
                          </button>
                        </Show>
                      </div>
                    )}
                  </For>
                </div>
              </Show>
            </Show>
          </section>
        </Show>
        <Show when={section() === "general"}>
          <section class={styles.settingsSection} aria-labelledby="settings-automation">
            <h3 id="settings-automation" class={styles.settingsSectionTitle}>
              Automation
            </h3>
            <label class={`${styles.settingsLabel} ${styles.shortField}`}>
              Purge delay
              <input
                type="text"
                class={styles.settingsInput}
                value={formatDuration(props.purgeDelay())}
                aria-describedby="purge-delay-description"
                onInput={(e) => {
                  e.currentTarget.setCustomValidity(
                    parseDuration(e.currentTarget.value) === null ? "Enter a duration such as 1m31s." : "",
                  );
                }}
                onChange={(e) => {
                  const delay = parseDuration(e.currentTarget.value);
                  if (delay === null) {
                    e.currentTarget.reportValidity();
                    return;
                  }
                  e.currentTarget.setCustomValidity("");
                  e.currentTarget.value = formatDuration(delay);
                  props.setPurgeDelay(delay);
                  void props.saveSettings({ purgeDelay: delay });
                }}
              />
            </label>
            <p id="purge-delay-description" class={styles.settingsDescription}>
              10s–24h before deletion.
            </p>
            <label class={styles.automationOption}>
              <input
                type="checkbox"
                checked={props.autoFixCI()}
                onChange={(e) => {
                  const val = e.currentTarget.checked;
                  props.setAutoFixCI(val);
                  void props.saveSettings({ autoFixOnCIFailure: val });
                }}
              />
              Auto-fix CI failures
            </label>
            <label class={styles.automationOption}>
              <input
                type="checkbox"
                checked={props.autoFixPR()}
                onChange={(e) => {
                  const val = e.currentTarget.checked;
                  props.setAutoFixPR(val);
                  void props.saveSettings({ autoFixOnPROpen: val });
                }}
              />
              Review and fix new PRs
            </label>
          </section>
        </Show>
        <Show when={section() === "server"}>
          <section class={styles.settingsSection} aria-labelledby="settings-models">
            <h3 id="settings-models" class={styles.settingsSectionTitle}>
              Reload models
            </h3>
            <div class={styles.modelRefreshActions}>
              <For
                each={refreshableHarnesses()}
                fallback={<p class={styles.settingsDescription}>No installed coding agents support model refresh.</p>}
              >
                {(harness) => (
                  <Button
                    type="button"
                    variant="gray"
                    disabled={props.refreshingHarness() !== null}
                    loading={props.refreshingHarness() === harness.name}
                    onClick={() => {
                      void props.refreshAvailableModels(harness.name);
                    }}
                  >
                    {harness.name}
                  </Button>
                )}
              </For>
            </div>
            <Show when={props.modelRefreshStatus()}>
              {(status) => (
                <p
                  class={styles.modelRefreshFeedback}
                  data-state={status().state}
                  role={status().state === "error" ? "alert" : "status"}
                  aria-label="Model reload status"
                >
                  <Show when={status().state === "error"}>
                    <ErrorIcon aria-hidden="true" />
                  </Show>
                  {status().message}
                </p>
              )}
            </Show>
          </section>
          <section class={styles.settingsSection} aria-labelledby="settings-version">
            <h3 id="settings-version" class={styles.settingsSectionTitle}>
              Version
            </h3>
            <Show
              when={props.versionInfo()}
              fallback={
                <Show
                  when={props.checkingUpdate()}
                  fallback={
                    <Show when={props.versionCheckError()}>
                      <p class={`${styles.settingsDescription} ${styles.settingsDescriptionError}`}>
                        Check failed: {props.versionCheckError()}
                      </p>
                    </Show>
                  }
                >
                  <p class={styles.settingsDescription}>Checking for updates…</p>
                </Show>
              }
            >
              {(v) => (
                <>
                  <p class={styles.settingsDescription}>
                    <strong>{v().current ? `caic v${v().current}` : "caic development build"}</strong>
                    <Show when={v().latest}>
                      {" — "}
                      <Show when={v().updateAvailable} fallback={<>latest: v{v().latest} (up to date)</>}>
                        latest: <strong>v{v().latest}</strong> (update available)
                      </Show>
                    </Show>
                  </p>
                  <Show when={v().checkError}>
                    <p class={`${styles.settingsDescription} ${styles.settingsDescriptionError}`}>
                      Check failed: {v().checkError}
                    </p>
                  </Show>
                  <Show when={v().autoUpdateEnabled && v().updateAvailable}>
                    <button
                      type="button"
                      class={styles.settingsButton}
                      disabled={props.updating()}
                      onClick={() => {
                        void props.triggerServerUpdate();
                      }}
                    >
                      {props.updating() ? "Updating…" : "Update now"}
                    </button>
                  </Show>
                  <Show when={props.updateStatus()}>
                    <p class={styles.settingsDescription}>{props.updateStatus()}</p>
                  </Show>
                </>
              )}
            </Show>
          </section>
        </Show>
      </div>
    </div>
  );
}
