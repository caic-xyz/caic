// SettingsPage is the /settings route, wiring application state into the settings form.

import SettingsForm from "../components/SettingsForm";
import { useAppState } from "../AppState";
import { Layout } from "../components/Layout";

export default function SettingsPage() {
  const s = useAppState();
  return (
    <Layout>
      <SettingsForm
        selectedImage={s.selectedImage}
        setSelectedImage={s.setSelectedImage}
        containerPlatform={s.containerPlatform}
        setContainerPlatform={s.setContainerPlatform}
        maxCPUs={s.maxCPUs}
        setMaxCPUs={s.setMaxCPUs}
        runtimes={s.runtimes}
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
        autoFixCI={s.autoFixCI}
        setAutoFixCI={s.setAutoFixCI}
        autoFixPR={s.autoFixPR}
        setAutoFixPR={s.setAutoFixPR}
        authProviders={s.auth.providers}
        oauthGrants={s.oauthGrants}
        oauthGrantError={s.oauthGrantError}
        revokingOAuthGrantID={s.revokingOAuthGrantID}
        revokeOAuthClientGrant={s.revokeOAuthClientGrant}
        versionInfo={s.versionInfo}
        versionCheckError={s.versionCheckError}
        checkingUpdate={s.checkingUpdate}
        updating={s.updating}
        updateStatus={s.updateStatus}
        saveSettings={s.saveSettings}
        triggerServerUpdate={s.triggerServerUpdate}
      />
    </Layout>
  );
}
