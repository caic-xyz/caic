// SettingsForm renders the application settings controls.

import { For, Index, Show, type Accessor, type Setter } from "solid-js";

import type { CacheMappingResp, CacheSize, OAuthGrantResp, MountMappingResp, Platform, RuntimeInfo, UpdatePreferencesReq, VersionResp, WellKnownCachesResp } from "@sdk/types.gen";

import styles from "./SettingsForm.module.css";

type SettingsOverrides = Partial<UpdatePreferencesReq["settings"]>;

function defaultContainerPath(hostPath: string, resolvedContainerPath?: string): string {
  if (!resolvedContainerPath) return "";
  if (hostPath === "~" || hostPath.startsWith("~/")) return hostPath;
  return resolvedContainerPath;
}

interface SettingsFormProps {
  selectedImage: Accessor<string>;
  setSelectedImage: Setter<string>;
  containerPlatform: Accessor<string>;
  setContainerPlatform: Setter<string>;
  maxCPUs: Accessor<number>;
  setMaxCPUs: Setter<number>;
  runtimes: Accessor<RuntimeInfo[]>;
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
  autoFixCI: Accessor<boolean>;
  setAutoFixCI: Setter<boolean>;
  autoFixPR: Accessor<boolean>;
  setAutoFixPR: Setter<boolean>;
  authProviders: Accessor<string[]>;
  oauthGrants: Accessor<OAuthGrantResp[]>;
  oauthGrantError: Accessor<string>;
  revokingOAuthGrantID: Accessor<string | null>;
  revokeOAuthClientGrant: (grantID: string) => Promise<void>;
  versionInfo: Accessor<VersionResp | null>;
  versionCheckError: Accessor<string>;
  checkingUpdate: Accessor<boolean>;
  updating: Accessor<boolean>;
  updateStatus: Accessor<string>;
  saveSettings: (overrides?: SettingsOverrides) => Promise<void>;
  triggerServerUpdate: () => Promise<void>;
}

export default function SettingsForm(props: SettingsFormProps) {
  const formatBytes = (bytes: number): string => {
    if (bytes <= 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    const i = Math.min(Math.floor(Math.log2(bytes) / 10), units.length - 1);
    const value = bytes / (1024 ** i);
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
  const updateCacheMapping = (index: number, update: Partial<CacheMappingResp>) => {
    props.setCacheMappings((prev) => prev.map((mapping, i) => (
      i === index ? { ...mapping, ...update, resolvedContainerPath: undefined } : mapping
    )));
  };
  const updateCustomMount = (index: number, update: Partial<MountMappingResp>) => {
    props.setCustomMounts((prev) => prev.map((mount, i) => (
      i === index ? { ...mount, ...update, resolvedContainerPath: undefined } : mount
    )));
  };

  return (
    <div class={styles.settingsPage}>
      <div class={styles.settingsPanel}>
        <h2 class={styles.settingsPanelTitle}>Settings</h2>
        <Show when={props.settingsError()}>
          <p class={styles.settingsError} role="alert">{props.settingsError()}</p>
        </Show>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Container</h3>
          <label class={styles.settingsLabel}>
            Docker image
            <input
              type="text"
              class={styles.settingsInput}
              placeholder="ghcr.io/caic-xyz/md-user:latest"
              value={props.selectedImage() || ""}
              onChange={(e) => props.setSelectedImage(e.currentTarget.value)}
              onBlur={() => {
                void props.saveSettings();
              }}
            />
          </label>
          <Show when={props.runtimes().length > 1}>
            <label class={styles.settingsLabel}>
              Runtime
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
                  {(rt) => <option value={rt.name} selected={rt.name === props.selectedRuntimeName()}>{rt.name}</option>}
                </For>
              </select>
            </label>
          </Show>
          <label class={styles.settingsLabel}>
            CPU architecture
            <select
              class={styles.settingsInput}
              value={props.containerPlatform()}
              onChange={(e) => {
                const val = e.currentTarget.value as Platform;
                props.setContainerPlatform(val);
                void props.saveSettings({ containerPlatform: val });
              }}
            >
              <option value="">Native</option>
              <option value="linux/amd64">linux/amd64</option>
              <option value="linux/arm64">linux/arm64</option>
            </select>
          </label>
          <label class={styles.settingsLabel}>
            CPU cores
            <input
              type="number"
              class={styles.settingsInput}
              placeholder="Default"
              min="0"
              value={props.maxCPUs() || ""}
              onChange={(e) => props.setMaxCPUs(parseInt(e.currentTarget.value, 10) || 0)}
              onBlur={() => {
                void props.saveSettings();
              }}
            />
          </label>
          <p class={styles.settingsDescription}>Maximum CPU cores for each container (0 = use default).</p>
        </div>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Well-known caches</h3>
          <div class={styles.cacheGrid}>
            <For each={props.wellKnownCachesList()}>
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
                      checked={isEnabled()}
                      onChange={(e) => {
                        const newCaches = { ...props.wellKnownCaches() };
                        newCaches[cache.name] = e.currentTarget.checked;
                        props.setWellKnownCaches(newCaches);
                        void props.saveSettings({ wellKnownCaches: newCaches as Record<string, boolean> });
                      }}
                    />
                    <span class={styles.cacheName}>{cache.name}</span>
                    <span class={styles.cacheSize} data-testid="cache-size">
                      {cacheSizeLabel(cache.name)}
                    </span>
                  </label>
                );
              }}
            </For>
          </div>
        </div>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Custom caches</h3>
          <p class={styles.settingsDescription}>Persistent host directories mounted into each container for tool caches. Leave the container path blank to use the same <code>~</code> path.</p>
          <Index each={props.cacheMappings()}>
            {(mapping, index) => (
              <div class={styles.cacheMappingRow} data-state={mapping().enabled ? "enabled" : "disabled"} data-testid="cache-mapping-row">
                <label class={styles.cacheMappingToggle} title="Enable custom cache">
                  <input
                    type="checkbox"
                    checked={mapping().enabled}
                    onChange={(e) => {
                      const enabled = e.currentTarget.checked;
                      const newMappings = props.cacheMappings().map((item, i) => (
                        i === index ? { ...item, enabled } : item
                      ));
                      props.setCacheMappings(newMappings);
                      void props.saveSettings({ cacheMappings: newMappings });
                    }}
                  />
                  <span class={styles.visuallyHidden}>Enable custom cache</span>
                </label>
                <span class={`${styles.mappingPathLabel} ${styles.mappingHostLabel}`}>Host path</span>
                <input
                  type="text"
                  class={`${styles.settingsInput} ${styles.mappingHostInput}`}
                  aria-label="Host path"
                  placeholder="~/.cache/tool"
                  value={mapping().hostPath}
                  onInput={(e) => updateCacheMapping(index, { hostPath: e.currentTarget.value })}
                  onBlur={() => {
                    void props.saveSettings();
                  }}
                />
                <span class={styles.cacheMappingArrow} data-testid="mapping-arrow">→</span>
                <span class={`${styles.mappingPathLabel} ${styles.mappingContainerLabel}`}>Container path</span>
                <input
                  type="text"
                  class={`${styles.settingsInput} ${styles.mappingContainerInput}`}
                  aria-label="Container path"
                  placeholder={defaultContainerPath(mapping().hostPath, mapping().resolvedContainerPath) || "Container path"}
                  value={mapping().containerPath}
                  onInput={(e) => updateCacheMapping(index, { containerPath: e.currentTarget.value })}
                  onBlur={() => {
                    void props.saveSettings();
                  }}
                />
                <Show when={mapping().containerPath === "" && defaultContainerPath(mapping().hostPath, mapping().resolvedContainerPath)}>
                  <span class={styles.mappingPathHint}>Uses {defaultContainerPath(mapping().hostPath, mapping().resolvedContainerPath)} by default</span>
                </Show>
                <button
                  type="button"
                  class={styles.cacheMappingRemove}
                  onClick={() => {
                    const newMappings = props.cacheMappings().filter((_, i) => i !== index);
                    props.setCacheMappings(newMappings);
                    void props.saveSettings({ cacheMappings: newMappings });
                  }}
                >
                  ×
                </button>
              </div>
            )}
          </Index>
          <button
            type="button"
            class={styles.settingsButton}
            onClick={() => {
              props.setCacheMappings([...props.cacheMappings(), { hostPath: "", containerPath: "", enabled: true }]);
            }}
          >
            + Add mapping
          </button>
        </div>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Custom mounts</h3>
          <p class={styles.settingsDescription}>Additional host directories mounted into each container. Leave the container path blank to use the same <code>~</code> path.</p>
          <Index each={props.customMounts()}>
            {(mount, index) => (
              <div class={styles.cacheMappingRow} data-state={mount().enabled ? "enabled" : "disabled"} data-testid="custom-mount-row">
                <label class={styles.cacheMappingToggle} title="Enable custom mount">
                  <input
                    type="checkbox"
                    checked={mount().enabled}
                    onChange={(e) => {
                      const enabled = e.currentTarget.checked;
                      const newMounts = props.customMounts().map((item, i) => (
                        i === index ? { ...item, enabled } : item
                      ));
                      props.setCustomMounts(newMounts);
                      void props.saveSettings({ customMounts: newMounts });
                    }}
                  />
                  <span class={styles.visuallyHidden}>Enable custom mount</span>
                </label>
                <span class={`${styles.mappingPathLabel} ${styles.mappingHostLabel}`}>Host path</span>
                <input
                  type="text"
                  class={`${styles.settingsInput} ${styles.mappingHostInput}`}
                  aria-label="Host path"
                  placeholder="~/Documents"
                  value={mount().hostPath}
                  onInput={(e) => updateCustomMount(index, { hostPath: e.currentTarget.value })}
                  onBlur={() => {
                    void props.saveSettings();
                  }}
                />
                <span class={styles.cacheMappingArrow} data-testid="mapping-arrow">→</span>
                <span class={`${styles.mappingPathLabel} ${styles.mappingContainerLabel}`}>Container path</span>
                <input
                  type="text"
                  class={`${styles.settingsInput} ${styles.mappingContainerInput}`}
                  aria-label="Container path"
                  placeholder={defaultContainerPath(mount().hostPath, mount().resolvedContainerPath) || "Container path"}
                  value={mount().containerPath}
                  onInput={(e) => updateCustomMount(index, { containerPath: e.currentTarget.value })}
                  onBlur={() => {
                    void props.saveSettings();
                  }}
                />
                <Show when={mount().containerPath === "" && defaultContainerPath(mount().hostPath, mount().resolvedContainerPath)}>
                  <span class={styles.mappingPathHint}>Uses {defaultContainerPath(mount().hostPath, mount().resolvedContainerPath)} by default</span>
                </Show>
                <label class={styles.mountOptionToggle} title="Mount read-only" data-testid="mount-read-only">
                  <input
                    type="checkbox"
                    checked={mount().readOnly ?? false}
                    onChange={(e) => {
                      const readOnly = e.currentTarget.checked;
                      const newMounts = props.customMounts().map((item, i) => (
                        i === index ? { ...item, readOnly } : item
                      ));
                      props.setCustomMounts(newMounts);
                      void props.saveSettings({ customMounts: newMounts });
                    }}
                  />
                  Read only
                </label>
                <button
                  type="button"
                  class={styles.cacheMappingRemove}
                  onClick={() => {
                    const newMounts = props.customMounts().filter((_, i) => i !== index);
                    props.setCustomMounts(newMounts);
                    void props.saveSettings({ customMounts: newMounts });
                  }}
                >
                  ×
                </button>
              </div>
            )}
          </Index>
          <button
            type="button"
            class={styles.settingsButton}
            onClick={() => {
              props.setCustomMounts([...props.customMounts(), { hostPath: "", containerPath: "", enabled: true, readOnly: false }]);
            }}
          >
            + Add mount
          </button>
        </div>
        <Show when={props.authProviders().length > 0}>
          <div class={styles.settingsSection}>
            <h3 class={styles.settingsSectionTitle}>MCP clients</h3>
            <p class={styles.settingsDescription}>Remote clients authorized to access caic through MCP OAuth.</p>
            <Show when={!props.oauthGrantError()} fallback={
              <p class={styles.settingsDescription} style={{ color: "var(--color-error)" }}>{props.oauthGrantError()}</p>
            }>
              <Show when={props.oauthGrants().length > 0} fallback={
                <p class={styles.settingsDescription}>No connected MCP clients.</p>
              }>
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
          </div>
        </Show>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Automation</h3>
          <label class={styles.settingsLabel}>
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
          <p class={styles.settingsDescription}>When CI fails on a PR and the agent has finished, automatically start a new task to fix it.</p>
          <label class={styles.settingsLabel}>
            <input
              type="checkbox"
              checked={props.autoFixPR()}
              onChange={(e) => {
                const val = e.currentTarget.checked;
                props.setAutoFixPR(val);
                void props.saveSettings({ autoFixOnPROpen: val });
              }}
            />
            Auto-fix PRs
          </label>
          <p class={styles.settingsDescription}>When a pull request is opened or reopened, automatically start a task to review and fix it.</p>
        </div>
        <div class={styles.settingsSection}>
          <h3 class={styles.settingsSectionTitle}>Version</h3>
          <Show when={props.versionInfo()} fallback={
            <Show when={props.checkingUpdate()} fallback={
              <Show when={props.versionCheckError()}>
                <p class={styles.settingsDescription} style={{ color: "var(--color-error)" }}>Check failed: {props.versionCheckError()}</p>
              </Show>
            }>
              <p class={styles.settingsDescription}>Checking for updates…</p>
            </Show>
          }>
            {(v) => (
              <>
                <p class={styles.settingsDescription}>
                  Current: <strong>caic v{v().current}</strong>
                  <Show when={v().latest}>
                    {" — "}
                    <Show when={v().updateAvailable} fallback={
                      <>latest: v{v().latest} (up to date)</>
                    }>
                      latest: <strong>v{v().latest}</strong> (update available)
                    </Show>
                  </Show>
                </p>
                <Show when={v().checkError}>
                  <p class={styles.settingsDescription} style={{ color: "var(--color-error)" }}>Check failed: {v().checkError}</p>
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
        </div>
      </div>
    </div>
  );
}
