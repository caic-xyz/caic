// SettingsPage is the /settings route, wiring application state into the settings form.

import SettingsForm from "../components/SettingsForm";
import { createEffect, onCleanup } from "solid-js";
import { useAppState } from "../AppState";
import { Layout } from "../components/Layout";

export default function SettingsPage() {
  const s = useAppState();
  createEffect(() => {
    const names = s.runtimes().map((runtime) => runtime.name);
    for (const name of names) void s.loadImageRefreshStatus(name);
    const poll = setInterval(() => {
      for (const name of names) void s.loadImageRefreshStatus(name);
    }, 10000);
    onCleanup(() => clearInterval(poll));
  });
  return (
    <Layout>
      <SettingsForm
        selectedImage={s.selectedImage}
        setSelectedImage={s.setSelectedImage}
        runtimeSettings={s.runtimeSettings}
        updateRuntimeSettings={s.updateRuntimeSettings}
        purgeDelay={s.purgeDelay}
        setPurgeDelay={s.setPurgeDelay}
        runtimes={s.runtimes}
        harnesses={s.harnesses}
        selectedRuntimeName={s.selectedRuntimeName}
        setSelectedRuntimeName={s.setSelectedRuntimeName}
        wellKnownCaches={s.wellKnownCaches}
        setWellKnownCaches={s.setWellKnownCaches}
        wellKnownCachesList={s.wellKnownCachesList}
        wellKnownCacheSizes={s.wellKnownCacheSizes}
        cacheMappings={s.cacheMappings}
        setCacheMappings={s.setCacheMappings}
        customMounts={s.customMounts}
        setCustomMounts={s.setCustomMounts}
        settingsError={s.settingsError}
        settingsSaveState={s.settingsSaveState}
        markSettingsDraft={s.markSettingsDraft}
        autoFixCI={s.autoFixCI}
        setAutoFixCI={s.setAutoFixCI}
        autoFixPR={s.autoFixPR}
        setAutoFixPR={s.setAutoFixPR}
        mcpOAuthAvailable={s.mcpOAuthAvailable}
        oauthGrants={s.oauthGrants}
        oauthGrantError={s.oauthGrantError}
        revokingOAuthGrantID={s.revokingOAuthGrantID}
        revokeOAuthClientGrant={s.revokeOAuthClientGrant}
        versionInfo={s.versionInfo}
        versionCheckError={s.versionCheckError}
        checkingUpdate={s.checkingUpdate}
        updating={s.updating}
        updateStatus={s.updateStatus}
        refreshingHarness={s.refreshingHarness}
        modelRefreshStatus={s.modelRefreshStatus}
        imageRefreshStatus={s.imageRefreshStatus}
        startImageRefresh={s.startImageRefresh}
        saveSettings={s.saveSettings}
        triggerServerUpdate={s.triggerServerUpdate}
        refreshAvailableModels={s.refreshAvailableModels}
      />
    </Layout>
  );
}
